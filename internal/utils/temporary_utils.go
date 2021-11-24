// TO BE REMOVED AFTER INTEGRATING ADDON INSTANCE CLIENT SDK

package utils

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	addonsv1alpha1 "github.com/openshift/addon-operator/apis/addons/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func SetAddonInstanceCondition(ctx context.Context, cacheBackedKubeClient client.Client, condition metav1.Condition, addonName string) error {
	addonInstance, err := getAddonInstanceByAddon(ctx, cacheBackedKubeClient, addonName)
	if err != nil {
		return fmt.Errorf("failed to fetch AddonInstance by the addon '%s': %w", addonName, err)
	}
	if err := upsertAddonInstanceCondition(ctx, cacheBackedKubeClient, &addonInstance, condition); err != nil {
		return fmt.Errorf("failed to update the conditions of the AddonInstance resource in the namespace %s: %w", addonInstance.Namespace, err)
	}
	return nil
}

// TODO(ykukreja): make the timeout tweakable or dynamically self-configurable depending on the AddonInstance's heartbeat update period
// TODO(ykukreja): implement the following function to comply with buffered channels as well
func CommunicateHeartbeat(heartbeatCommunicatorChannel chan metav1.Condition, condition metav1.Condition, log logr.Logger) {
	go func() {
		select {
		case heartbeatCommunicatorChannel <- condition:
		case <-time.After(30 * time.Second):
			fmt.Println("heartbeat couldn't be sent due to it not being received by a receiver due to the channel being choked!") // just a placeholder message :P
		}
	}()
}

func GetAddonInstanceConfiguration(ctx context.Context, cacheBackedKubeClient client.Client, addonName string) (addonsv1alpha1.AddonInstanceSpec, error) {
	addonInstance, err := getAddonInstanceByAddon(ctx, cacheBackedKubeClient, addonName)
	if err != nil {
		return addonsv1alpha1.AddonInstanceSpec{}, fmt.Errorf("failed to fetch AddonInstance by the addon '%s': %w", addonName, err)
	}
	return addonInstance.Spec, nil
}

func getAddonInstanceByAddon(ctx context.Context, cacheBackedKubeClient client.Client, addonName string) (addonsv1alpha1.AddonInstance, error) {
	addon := &addonsv1alpha1.Addon{}
	if err := cacheBackedKubeClient.Get(ctx, types.NamespacedName{Name: addonName}, addon); err != nil {
		return addonsv1alpha1.AddonInstance{}, err
	}
	targetNamespace, err := parseTargetNamespaceFromAddon(*addon)
	if err != nil {
		return addonsv1alpha1.AddonInstance{}, fmt.Errorf("failed to parse the target namespace from the Addon: %w", err)
	}
	addonInstance := &addonsv1alpha1.AddonInstance{}
	if err := cacheBackedKubeClient.Get(ctx, types.NamespacedName{Name: addonsv1alpha1.DefaultAddonInstanceName, Namespace: targetNamespace}, addonInstance); err != nil {
		return addonsv1alpha1.AddonInstance{}, fmt.Errorf("failed to fetch the AddonInstance resource in the namespace %s: %w", targetNamespace, err)
	}
	return *addonInstance, nil
}

func parseTargetNamespaceFromAddon(addon addonsv1alpha1.Addon) (string, error) {
	var targetNamespace string
	switch addon.Spec.Install.Type {
	case addonsv1alpha1.OLMOwnNamespace:
		if addon.Spec.Install.OLMOwnNamespace == nil ||
			len(addon.Spec.Install.OLMOwnNamespace.Namespace) == 0 {
			// invalid/missing configuration
			return "", fmt.Errorf(".install.spec.olmOwmNamespace.namespace not found")
		}
		targetNamespace = addon.Spec.Install.OLMOwnNamespace.Namespace

	case addonsv1alpha1.OLMAllNamespaces:
		if addon.Spec.Install.OLMAllNamespaces == nil ||
			len(addon.Spec.Install.OLMAllNamespaces.Namespace) == 0 {
			// invalid/missing configuration
			return "", fmt.Errorf(".install.spec.olmAllNamespaces.namespace not found")
		}
		targetNamespace = addon.Spec.Install.OLMAllNamespaces.Namespace
	default:
		// ideally, this should never happen
		// but technically, it is possible to happen if validation webhook is turned off and CRD validation gets bypassed via the `--validate=false` argument
		return "", fmt.Errorf("unsupported install type found: %s", addon.Spec.Install.Type)
	}
	return targetNamespace, nil
}

func upsertAddonInstanceCondition(ctx context.Context, cacheBackedKubeClient client.Client, addonInstance *addonsv1alpha1.AddonInstance, condition metav1.Condition) error {
	currentTime := metav1.Now()
	if condition.LastTransitionTime.IsZero() {
		condition.LastTransitionTime = currentTime
	}
	// TODO: confirm that it's not worth tracking the ObservedGeneration at per-condition basis
	meta.SetStatusCondition(&(*addonInstance).Status.Conditions, condition)
	addonInstance.Status.ObservedGeneration = (*addonInstance).Generation
	addonInstance.Status.LastHeartbeatTime = metav1.Now()
	return cacheBackedKubeClient.Status().Update(ctx, addonInstance)
}
