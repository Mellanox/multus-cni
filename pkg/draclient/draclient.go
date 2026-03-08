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

const (
	multusDeviceIDAttr     = "k8s.cni.cncf.io/deviceID"
	multusResourceNameAttr = "k8s.cni.cncf.io/resourceName"
)

type deviceInfo struct {
	DeviceID     string
	ResourceName string
}

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

	for _, claimResource := range pod.Status.ResourceClaimStatuses { // (resourceClaimName/RequestedDevice)
		claimName := *claimResource.ResourceClaimName
		logging.Debugf("GetPodResourceMap: processing resource claim: %s", claimName)

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

			info, err := d.getDeviceInfo(ctx, result)
			if err != nil {
				logging.Errorf("GetPodResourceMap: failed to get device info for claim %s: %v", claimName, err)
				return err
			}

			// Prefer the convention resourceName attribute as the map key so that
			// NAD annotations can use a static name (like the old device-plugin model).
			// Fall back to claim-name/request-name when the attribute is absent.
			resourceMapKey := info.ResourceName
			if resourceMapKey == "" {
				resourceMapKey = fmt.Sprintf("%s/%s", claimName, result.Request)
				logging.Debugf("GetPodResourceMap: no %s attribute, falling back to key %s", multusResourceNameAttr, resourceMapKey)
			}

			if rInfo, ok := resourceMap[resourceMapKey]; ok {
				rInfo.DeviceIDs = append(rInfo.DeviceIDs, info.DeviceID)
				logging.Debugf("GetPodResourceMap: appended device ID %s to existing resource map entry %s", info.DeviceID, resourceMapKey)
			} else {
				resourceMap[resourceMapKey] = &types.ResourceInfo{DeviceIDs: []string{info.DeviceID}}
				logging.Debugf("GetPodResourceMap: created new resource map entry %s with device ID %s", info.DeviceID, resourceMapKey)
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
// from requestMappings[].resourceName, so the same NAD annotation works as in the
// device-plugin model.
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

	resultsByRequest := make(map[string]resourcev1api.DeviceRequestAllocationResult)
	for _, result := range resourceClaim.Status.Allocation.Devices.Results {
		resultsByRequest[result.Request] = result
	}

	for _, mapping := range extStatus.RequestMappings {
		result, ok := resultsByRequest[mapping.RequestName]
		if !ok {
			logging.Errorf("GetPodResourceMap: extended resource request %s not found in claim %s", mapping.RequestName, claimName)
			return fmt.Errorf("request %s not found in claim %s", mapping.RequestName, claimName)
		}

		info, err := d.getDeviceInfo(ctx, result)
		if err != nil {
			logging.Errorf("GetPodResourceMap: failed to get device info for extended resource claim %s request %s: %v", claimName, mapping.RequestName, err)
			return err
		}

		resourceMapKey := mapping.ResourceName
		if rInfo, ok := resourceMap[resourceMapKey]; ok {
			rInfo.DeviceIDs = append(rInfo.DeviceIDs, info.DeviceID)
			logging.Debugf("GetPodResourceMap: appended device ID %s to extended resource map entry %s", info.DeviceID, resourceMapKey)
		} else {
			resourceMap[resourceMapKey] = &types.ResourceInfo{DeviceIDs: []string{info.DeviceID}}
			logging.Debugf("GetPodResourceMap: created new extended resource map entry %s with device ID %s", resourceMapKey, info.DeviceID)
		}
	}

	logging.Debugf("GetPodResourceMap: successfully processed extended resource claim %s", claimName)
	return nil
}

func (d *draClient) getDeviceInfo(ctx context.Context, result resourcev1api.DeviceRequestAllocationResult) (*deviceInfo, error) {
	key := fmt.Sprintf("%s/%s", result.Driver, result.Pool)
	logging.Debugf("getDeviceInfo: looking up device for driver/pool: %s, device: %s", key, result.Device)

	resourceSlice, ok := d.resourceSliceCache[key]
	if !ok {
		logging.Debugf("getDeviceInfo: resource slice %s not in cache, fetching from API", key)
		listOptions := metav1.ListOptions{}
		allResourceSlices, err := d.client.ResourceSlices().List(ctx, listOptions)
		if err != nil {
			logging.Errorf("getDeviceInfo: failed to list resource slices: %v", err)
			return nil, err
		}

		var matchingSlices []*resourcev1api.ResourceSlice
		for i := range allResourceSlices.Items {
			slice := &allResourceSlices.Items[i]
			if slice.Spec.Driver == result.Driver && slice.Spec.Pool.Name == result.Pool {
				matchingSlices = append(matchingSlices, slice)
			}
		}

		if len(matchingSlices) == 0 {
			logging.Errorf("getDeviceInfo: expected 1 resource slice for %s, got 0: no resource slice found", key)
			return nil, fmt.Errorf("expected 1 resource slice, got 0: no resource slice found")
		}
		if len(matchingSlices) > 1 {
			logging.Errorf("getDeviceInfo: expected 1 resource slice for %s, got %d", key, len(matchingSlices))
			return nil, fmt.Errorf("expected 1 resource slice, got %d", len(matchingSlices))
		}
		resourceSlice = matchingSlices[0]
		d.resourceSliceCache[key] = resourceSlice
		logging.Debugf("getDeviceInfo: cached resource slice %s with %d devices", key, len(resourceSlice.Spec.Devices))
	} else {
		logging.Debugf("getDeviceInfo: using cached resource slice %s", key)
	}

	logging.Debugf("getDeviceInfo: searching for device %s in %d devices", result.Device, len(resourceSlice.Spec.Devices))
	for _, device := range resourceSlice.Spec.Devices {
		if device.Name != result.Device {
			continue
		}
		logging.Debugf("getDeviceInfo: found device %s, checking attributes", device.Name)

		devIDAttr, exists := device.Attributes[multusDeviceIDAttr]
		if !exists {
			logging.Debugf("getDeviceInfo: device %s does not have %s attribute", device.Name, multusDeviceIDAttr)
			continue
		}

		info := &deviceInfo{DeviceID: *devIDAttr.StringValue}

		if resNameAttr, ok := device.Attributes[multusResourceNameAttr]; ok && resNameAttr.StringValue != nil {
			info.ResourceName = *resNameAttr.StringValue
			logging.Debugf("getDeviceInfo: device %s has resourceName %s", device.Name, info.ResourceName)
		}

		logging.Verbosef("getDeviceInfo: successfully retrieved info for device %s (driver/pool: %s): deviceID=%s, resourceName=%s",
			result.Device, key, info.DeviceID, info.ResourceName)
		return info, nil
	}

	err := fmt.Errorf("device %s not found for claim resource %s/%s", result.Device, result.Driver, result.Pool)
	logging.Errorf("getDeviceInfo: %v", err)
	return nil, err
}
