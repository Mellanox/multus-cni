package draclient

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	resourcev1api "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourcev1 "k8s.io/client-go/kubernetes/typed/resource/v1"

	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/logging"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/types"
)

type ClientInterface interface {
	GetPodResourceMap(pod *v1.Pod, resourceMap map[string]*types.ResourceInfo) error
}

type draClient struct {
	client             resourcev1.ResourceV1Interface
	resourceSliceCache map[string]*resourcev1api.ResourceSlice
	resourceClaimCache map[string]*resourcev1api.ResourceClaim
}

func NewClient(client resourcev1.ResourceV1Interface) ClientInterface {
	logging.Debugf("NewClient: creating new DRA client")
	return &draClient{
		client:             client,
		resourceSliceCache: make(map[string]*resourcev1api.ResourceSlice),
		resourceClaimCache: make(map[string]*resourcev1api.ResourceClaim),
	}
}

func (d *draClient) GetPodResourceMap(pod *v1.Pod, resourceMap map[string]*types.ResourceInfo) error {
	var err error
	logging.Verbosef("GetPodResourceMap: processing DRA resources for pod %s/%s", pod.Namespace, pod.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, claimResource := range pod.Status.ResourceClaimStatuses {
		// Use generated claim name to fetch the ResourceClaim from the API
		claimName := *claimResource.ResourceClaimName
		// Use spec ref name (claimResource.Name) for the resource map key so NAD annotations
		// can use a stable key like "sriov-port1/vf1" that matches pod.spec.resourceClaims[].name.
		claimRefName := claimResource.Name
		logging.Debugf("GetPodResourceMap: processing resource claim: %s (ref name: %s)", claimName, claimRefName)

		// get resource claim
		resourceClaim, ok := d.resourceClaimCache[claimName]
		if !ok {
			logging.Debugf("GetPodResourceMap: resource claim %s not in cache, fetching from API", claimName)
			resourceClaim, err = d.client.ResourceClaims(pod.Namespace).Get(ctx, *claimResource.ResourceClaimName, metav1.GetOptions{})
			if err != nil {
				logging.Errorf("GetPodResourceMap: failed to get resource claim %s: %v", claimName, err)
				return err
			}
			d.resourceClaimCache[claimName] = resourceClaim
			logging.Debugf("GetPodResourceMap: cached resource claim %s", claimName)
		} else {
			logging.Debugf("GetPodResourceMap: using cached resource claim %s", claimName)
		}

		for _, result := range resourceClaim.Status.Allocation.Devices.Results {
			logging.Debugf("GetPodResourceMap: processing device allocation - driver: %s, pool: %s, device: %s, request: %s",
				result.Driver, result.Pool, result.Device, result.Request)

			deviceID, err := d.getDeviceID(ctx, result)
			if err != nil {
				logging.Errorf("GetPodResourceMap: failed to get device ID for claim %s: %v", claimName, err)
				return err
			}

			resourceMapKey := fmt.Sprintf("%s/%s", claimRefName, result.Request)
			if rInfo, ok := resourceMap[resourceMapKey]; ok {
				rInfo.DeviceIDs = append(rInfo.DeviceIDs, deviceID)
				logging.Debugf("GetPodResourceMap: appended device ID %s to existing resource map entry %s", deviceID, resourceMapKey)
			} else {
				resourceMap[resourceMapKey] = &types.ResourceInfo{DeviceIDs: []string{deviceID}}
				logging.Debugf("GetPodResourceMap: created new resource map entry %s with device ID %s", resourceMapKey, deviceID)
			}
		}
		logging.Debugf("GetPodResourceMap: successfully processed resource claim %s", claimName)
	}

	// Process ExtendedResourceClaimStatus (pods using extended resource feature gate)
	if pod.Status.ExtendedResourceClaimStatus != nil {
		if err := d.processExtendedResourceClaimStatus(ctx, pod, resourceMap); err != nil {
			return err
		}
	}

	logging.Verbosef("GetPodResourceMap: successfully processed all DRA resources for pod %s/%s, total resources: %d",
		pod.Namespace, pod.Name, len(resourceMap))
	return nil
}

// processExtendedResourceClaimStatus fills the resource map for pods that use
// the extended resource feature gate (pod.Status.ExtendedResourceClaimStatus).
// The resource map key is the extended resource name (e.g. example.com/sriov-port1)
// from requestMappings[].resourceName.
func (d *draClient) processExtendedResourceClaimStatus(ctx context.Context, pod *v1.Pod, resourceMap map[string]*types.ResourceInfo) error {
	extStatus := pod.Status.ExtendedResourceClaimStatus
	claimName := extStatus.ResourceClaimName
	logging.Debugf("GetPodResourceMap: processing extended resource claim: %s", claimName)

	resourceClaim, ok := d.resourceClaimCache[claimName]
	if !ok {
		var err error
		resourceClaim, err = d.client.ResourceClaims(pod.Namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			logging.Errorf("GetPodResourceMap: failed to get extended resource claim %s: %v", claimName, err)
			return err
		}
		d.resourceClaimCache[claimName] = resourceClaim
		logging.Debugf("GetPodResourceMap: cached extended resource claim %s", claimName)
	}

	if resourceClaim.Status.Allocation == nil || resourceClaim.Status.Allocation.Devices.Results == nil {
		logging.Errorf("GetPodResourceMap: claim %s has no device allocation", claimName)
		return fmt.Errorf("claim %s has no device allocation", claimName)
	}

	// Group allocation results by request name (a request can have count > 1, so multiple results per request)
	resultsByRequest := make(map[string][]resourcev1api.DeviceRequestAllocationResult)
	for _, result := range resourceClaim.Status.Allocation.Devices.Results {
		resultsByRequest[result.Request] = append(resultsByRequest[result.Request], result)
	}

	for _, mapping := range extStatus.RequestMappings {
		results, ok := resultsByRequest[mapping.RequestName]
		if !ok || len(results) == 0 {
			logging.Errorf("GetPodResourceMap: extended resource request %s not found in claim %s", mapping.RequestName, claimName)
			return fmt.Errorf("request %s not found in claim %s", mapping.RequestName, claimName)
		}

		resourceMapKey := mapping.ResourceName
		for _, result := range results {
			deviceID, err := d.getDeviceID(ctx, result)
			if err != nil {
				logging.Errorf("GetPodResourceMap: failed to get device info for extended resource claim %s request %s: %v", claimName, mapping.RequestName, err)
				return err
			}

			if rInfo, ok := resourceMap[resourceMapKey]; ok {
				rInfo.DeviceIDs = append(rInfo.DeviceIDs, deviceID)
				logging.Debugf("GetPodResourceMap: appended device ID %s to extended resource map entry %s", deviceID, resourceMapKey)
			} else {
				resourceMap[resourceMapKey] = &types.ResourceInfo{DeviceIDs: []string{deviceID}}
				logging.Debugf("GetPodResourceMap: created new extended resource map entry %s with device ID %s", resourceMapKey, deviceID)
			}
		}
	}

	logging.Debugf("GetPodResourceMap: successfully processed extended resource claim %s", claimName)
	return nil
}

func (d *draClient) getDeviceID(ctx context.Context, result resourcev1api.DeviceRequestAllocationResult) (string, error) {
	key := fmt.Sprintf("%s/%s", result.Driver, result.Pool)
	logging.Debugf("getDeviceID: looking up device ID for driver/pool: %s, device: %s", key, result.Device)

	resourceSlice, ok := d.resourceSliceCache[key]
	if !ok {
		logging.Debugf("getDeviceID: resource slice %s not in cache, fetching from API", key)
		// List all ResourceSlices - field selectors are not supported for spec.driver and spec.pool.name
		listOptions := metav1.ListOptions{}
		allResourceSlices, err := d.client.ResourceSlices().List(ctx, listOptions)
		if err != nil {
			logging.Errorf("getDeviceID: failed to list resource slices: %v", err)
			return "", err
		}

		var matchingSlices []*resourcev1api.ResourceSlice
		for i := range allResourceSlices.Items {
			slice := &allResourceSlices.Items[i]
			if slice.Spec.Driver == result.Driver && slice.Spec.Pool.Name == result.Pool {
				matchingSlices = append(matchingSlices, slice)
			}
		}

		if len(matchingSlices) == 0 {
			logging.Errorf("getDeviceID: expected 1 resource slice for %s, got 0: no resource slice found", key)
			return "", fmt.Errorf("expected 1 resource slice, got 0: no resource slice found")
		}
		if len(matchingSlices) > 1 {
			logging.Errorf("getDeviceID: expected 1 resource slice for %s, got %d", key, len(matchingSlices))
			return "", fmt.Errorf("expected 1 resource slice, got %d", len(matchingSlices))
		}
		resourceSlice = matchingSlices[0]
		d.resourceSliceCache[key] = resourceSlice
		logging.Debugf("getDeviceID: cached resource slice %s with %d devices", key, len(resourceSlice.Spec.Devices))
	} else {
		logging.Debugf("getDeviceID: using cached resource slice %s", key)
	}

	logging.Debugf("getDeviceID: searching for device %s in %d devices", result.Device, len(resourceSlice.Spec.Devices))
	for _, device := range resourceSlice.Spec.Devices {
		if device.Name != result.Device {
			continue
		}
		logging.Debugf("getDeviceID: found device %s, checking for deviceID attribute", device.Name)
		deviceID, exists := device.Attributes["k8s.cni.cncf.io/deviceID"]
		if !exists {
			logging.Debugf("getDeviceID: device %s does not have k8s.cni.cncf.io/deviceID attribute", device.Name)
			continue
		}
		logging.Verbosef("getDeviceID: successfully retrieved device ID %s for device %s (driver/pool: %s)",
			*deviceID.StringValue, result.Device, key)
		return *deviceID.StringValue, nil
	}

	err := fmt.Errorf("device %s not found for claim resource %s/%s", result.Device, result.Driver, result.Pool)
	logging.Errorf("getDeviceID: %v", err)
	return "", err
}
