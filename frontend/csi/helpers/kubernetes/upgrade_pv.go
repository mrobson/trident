package kubernetes

import (
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/netapp/trident/frontend/csi"
	"github.com/netapp/trident/storage"
)

/////////////////////////////////////////////////////////////////////////////
//
// This file contains the code to convert NFS/iSCSI PVs to CSI PVs.
//
/////////////////////////////////////////////////////////////////////////////

func (p *Plugin) UpgradeVolume(request *storage.UpgradeVolumeRequest) (*storage.VolumeExternal, error) {

	log.WithFields(log.Fields{
		"volume": request.Volume,
		"type":   request.Type,
	}).Infof("PV upgrade: workflow started.")

	// Check volume exists in Trident
	volume, err := p.orchestrator.GetVolume(request.Volume)
	if err != nil {
		message := "PV upgrade: could not find the volume to upgrade"
		log.WithFields(log.Fields{
			"Volume": request.Volume,
			"error":  err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithFields(log.Fields{
		"volume": volume.Config.Name,
		"type":   request.Type,
	}).Infof("PV upgrade: volume found.")

	// Check volume state is online
	if volume.State != storage.VolumeStateOnline {
		message := "PV upgrade: Trident volume to be upgraded must be in online state"
		log.WithFields(log.Fields{
			"Volume": volume.Config.Name,
			"State":  volume.State,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s", message)
	}
	log.WithField("volume", volume.Config.Name).Debug("PV upgrade: Trident volume is online.")

	// Get PV
	pv, err := p.getCachedPVByName(request.Volume)
	if err != nil {
		message := "PV upgrade: could not find the PV to upgrade"
		log.WithFields(log.Fields{
			"PV":    request.Volume,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithField("PV", pv.Name).Debug("PV upgrade: PV found in cache.")

	// Check volume type is iSCSI or NFS
	if pv.Spec.NFS == nil && pv.Spec.ISCSI == nil {
		message := "PV to be upgraded must be of type NFS or iSCSI"
		log.WithField("PV", pv.Name).Errorf("%s.", message)
		return nil, fmt.Errorf("%s", message)
	} else if pv.Spec.NFS != nil {
		log.WithField("PV", pv.Name).Debug("PV upgrade: volume is NFS.")
	} else if pv.Spec.ISCSI != nil {
		log.WithField("PV", pv.Name).Debug("PV upgrade: volume is iSCSI.")
	}

	// Check PV is bound to a PVC
	if pv.Status.Phase != v1.VolumeBound {
		message := "PV upgrade: PV must be bound to a PVC"
		log.WithField("PV", pv.Name).Errorf("%s.", message)
		return nil, fmt.Errorf("%s", message)
	}
	log.WithField("PV", pv.Name).Debug("PV upgrade: PV state is Bound.")

	// Ensure the legacy PV was provisioned by Trident
	if pv.ObjectMeta.Annotations[AnnDynamicallyProvisioned] != csi.LegacyProvisioner {
		message := "PV upgrade: PV must have been provisioned by Trident"
		log.WithFields(log.Fields{
			"PV":          pv.Name,
			"provisioner": csi.LegacyProvisioner,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s", message)
	}
	log.WithFields(log.Fields{
		"PV":          pv.Name,
		"provisioner": csi.LegacyProvisioner,
	}).Debug("PV upgrade: PV was provisioned by Trident.")

	namespace := pv.Spec.ClaimRef.Namespace
	pvcDisplayName := namespace + "/" + pv.Spec.ClaimRef.Name

	// Get PVC
	pvc, err := p.getCachedPVCByName(pv.Spec.ClaimRef.Name, pv.Spec.ClaimRef.Namespace)
	if err != nil {
		message := "PV upgrade: could not find the PVC bound to the PV"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"PVC":   pvcDisplayName,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithFields(log.Fields{
		"PV":  pv.Name,
		"PVC": pvcDisplayName,
	}).Debug("PV upgrade: PVC found in cache.")

	// Ensure no naked pods have PV mounted.  Owned pods will be deleted later in the workflow.
	ownedPodsForPVC, nakedPodsForPVC, err := p.getPodsForPVC(pvc)
	if err != nil {
		message := "PV upgrade: could not check for pods using the PV"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"PVC":   pvcDisplayName,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	} else if len(nakedPodsForPVC) > 0 {
		message := fmt.Sprintf("PV upgrade: one or more naked pods are using the PV (%s); "+
			"shut down these pods manually and try again", strings.Join(nakedPodsForPVC, ","))
		log.WithFields(log.Fields{
			"PV":  pv.Name,
			"PVC": pvcDisplayName,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s", message)
	} else if len(ownedPodsForPVC) > 0 {
		log.WithFields(log.Fields{
			"PV":   pv.Name,
			"PVC":  pvcDisplayName,
			"pods": strings.Join(ownedPodsForPVC, ","),
		}).Info("PV upgrade: one or more owned pods are using the PV.")
	} else {
		log.WithFields(log.Fields{
			"PV":  pv.Name,
			"PVC": pvcDisplayName,
		}).Info("PV upgrade: no owned pods are using the PV.")
	}

	// Check that PV has at most one finalizer, which must be kubernetes.io/pv-protection
	if pv.Finalizers != nil && len(pv.Finalizers) > 0 {
		if pv.Finalizers[0] != FinalizerPVProtection || len(pv.Finalizers) > 1 {
			message := "PV upgrade: PV has a finalizer other than kubernetes.io/pv-protection"
			log.WithField("PV", pv.Name).Errorf("%s.", message)
			return nil, fmt.Errorf("%s", message)
		}
	}

	// TODO: Set upgrading state on volume
	// TODO: Save PV & PVC transactions
	// TODO: Set up deferred error handling

	// Delete the PV along with any finalizers
	if err := p.deletePVForUpgrade(pv); err != nil {
		message := "PV upgrade: could not delete the PV"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithField("PV", pv.Name).Infof("PV upgrade: PV deleted.")

	// Wait for PVC to become Lost
	lostPVC, err := p.waitForPVCPhase(pvc, v1.ClaimLost, PVDeleteWaitPeriod)
	if err != nil {
		message := "PV upgrade: PVC did not reach the Lost state"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"PVC":   pvcDisplayName,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithFields(log.Fields{
		"PV":    pv.Name,
		"PVC":   pvcDisplayName,
		"error": err,
	}).Infof("PV upgrade: PVC reached the Lost state.")

	// Delete all owned pods that were using the PV
	for _, podName := range ownedPodsForPVC {

		// Delete pod
		if err := p.kubeClient.CoreV1().Pods(namespace).Delete(podName, &metav1.DeleteOptions{}); err != nil {
			message := "PV upgrade: could not delete a pod using the PV"
			log.WithFields(log.Fields{
				"PV":    pv.Name,
				"PVC":   pvcDisplayName,
				"pod":   podName,
				"error": err,
			}).Errorf("%s.", message)
			return nil, fmt.Errorf("%s: %v", message, err)
		} else {
			log.WithFields(log.Fields{
				"PV":  pv.Name,
				"PVC": pvcDisplayName,
				"pod": podName,
			}).Infof("PV upgrade: Owned pod deleted.")
		}
	}

	// Wait for all deleted pods to disappear (or reappear in a non-Running state)
	for _, podName := range ownedPodsForPVC {

		// Wait for pod to disappear or become pending
		if _, err := p.waitForDeletedOrNonRunningPod(podName, namespace, PodDeleteWaitPeriod); err != nil {
			message := "PV upgrade: unexpected pod status"
			log.WithFields(log.Fields{
				"PV":    pv.Name,
				"PVC":   pvcDisplayName,
				"pod":   podName,
				"error": err,
			}).Errorf("%s.", message)
			return nil, fmt.Errorf("%s: %v", message, err)
		} else {
			log.WithFields(log.Fields{
				"PV":  pv.Name,
				"PVC": pvcDisplayName,
				"pod": podName,
			}).Info("PV upgrade: Pod deleted or non-Running.")
		}
	}

	// TODO: Do controller stuff (igroups, etc.) (?)

	// Remove bind-completed annotation from PVC
	unboundLostPVC, err := p.removePVCBindCompletedAnnotation(lostPVC)
	if err != nil {
		message := "PV upgrade: could not remove bind-completed annotation from PVC"
		log.WithFields(log.Fields{
			"PVC":   pvcDisplayName,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithField("PVC", pvc.Name).Info("PV upgrade: removed bind-completed annotation from PVC.")

	// Create new PV
	csiPV, err := p.createCSIPVFromPV(pv, volume)
	if err != nil {
		message := "PV upgrade: could not create the CSI version of PV being upgraded"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"error": err,
		}).Errorf("PV upgrade: %s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	}
	log.WithField("PV", csiPV.Name).Info("PV upgrade: created CSI version of PV.")

	// Wait for PVC to become Bound
	boundPVC, err := p.waitForPVCPhase(unboundLostPVC, v1.ClaimBound, PVDeleteWaitPeriod)
	if err != nil {
		message := "PV upgrade: PVC did not reach the Bound state"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"PVC":   pvcDisplayName,
			"error": err,
		}).Errorf("%s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)
	} else if boundPVC != nil {
		log.WithFields(log.Fields{
			"PV":  csiPV.Name,
			"PVC": pvcDisplayName,
		}).Infof("PV upgrade: PVC bound.")
	}

	// TODO: Clear upgrading state on volume
	// TODO: Clean up saved info

	// Return volume to caller
	return volume, nil
}

func (p *Plugin) deletePVForUpgrade(pv *v1.PersistentVolume) error {

	// Check is PV has finalizers
	hasFinalizers := pv.Finalizers != nil && len(pv.Finalizers) > 0

	// Delete PV if it doesn't have a deletion timestamp
	if pv.DeletionTimestamp == nil {

		// PV hasn't been deleted yet, so send the delete
		if err := p.kubeClient.CoreV1().PersistentVolumes().Delete(pv.Name, &metav1.DeleteOptions{}); err != nil {
			return err
		}

		// Wait for disappearance (unlikely) or a deletion timestamp (PV pinned by finalizer)
		if deletedPV, err := p.waitForDeletedPV(pv.Name, PVDeleteWaitPeriod); err != nil {
			return err
		} else if deletedPV != nil {
			if _, err := p.removePVFinalizers(deletedPV); err != nil {
				return err
			}
		}
	} else {

		// PV was deleted previously, so just remove any finalizer so it can be fully deleted
		if hasFinalizers {
			if _, err := p.removePVFinalizers(pv); err != nil {
				return err
			}
		}
	}

	// Wait for PV to have deletion timestamp
	if err := p.waitForPVDisappearance(pv.Name, PVDeleteWaitPeriod); err != nil {
		return err
	}

	return nil
}

// waitForDeletedPV waits for a PV to be deleted.  The function can return multiple combinations:
//    (nil, nil)   --> the PV disappeared from the cache
//    (PV, nil)    --> the PV's deletedTimestamp is set (it may have finalizers set)
//    (nil, error) --> an error occurred checking for the PV in the cache
//    (PV, error)  --> the PV was not deleted before the retry loop timed out
//
func (p *Plugin) waitForDeletedPV(name string, maxElapsedTime time.Duration) (*v1.PersistentVolume, error) {

	var pv *v1.PersistentVolume
	var ok bool

	checkForDeletedPV := func() error {
		pv = nil
		if item, exists, err := p.pvIndexer.GetByKey(name); err != nil {
			return err
		} else if !exists {
			return nil
		} else if pv, ok = item.(*v1.PersistentVolume); !ok {
			return fmt.Errorf("non-PV object %s found in cache", name)
		} else if pv.DeletionTimestamp == nil {
			return fmt.Errorf("PV %s deletion timestamp not set", name)
		}
		return nil
	}
	pvNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"pv":        name,
			"increment": duration,
		}).Debugf("PV not yet deleted, waiting.")
	}
	pvBackoff := backoff.NewExponentialBackOff()
	pvBackoff.InitialInterval = CacheBackoffInitialInterval
	pvBackoff.RandomizationFactor = CacheBackoffRandomizationFactor
	pvBackoff.Multiplier = CacheBackoffMultiplier
	pvBackoff.MaxInterval = CacheBackoffMaxInterval
	pvBackoff.MaxElapsedTime = maxElapsedTime

	if err := backoff.RetryNotify(checkForDeletedPV, pvBackoff, pvNotify); err != nil {
		return nil, fmt.Errorf("PV %s was not deleted after %3.2f seconds", name, maxElapsedTime.Seconds())
	}

	return pv, nil
}

// waitForPVDisappearance waits for a PV to be fully deleted and gone from the cache.
func (p *Plugin) waitForPVDisappearance(name string, maxElapsedTime time.Duration) error {

	checkForDeletedPV := func() error {
		if item, exists, err := p.pvIndexer.GetByKey(name); err != nil {
			return err
		} else if !exists {
			return nil
		} else if _, ok := item.(*v1.PersistentVolume); !ok {
			return fmt.Errorf("non-PV object %s found in cache", name)
		} else {
			return fmt.Errorf("PV %s still exists", name)
		}
	}
	pvNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"pv":        name,
			"increment": duration,
		}).Debugf("PV not yet fully deleted, waiting.")
	}
	pvBackoff := backoff.NewExponentialBackOff()
	pvBackoff.InitialInterval = CacheBackoffInitialInterval
	pvBackoff.RandomizationFactor = CacheBackoffRandomizationFactor
	pvBackoff.Multiplier = CacheBackoffMultiplier
	pvBackoff.MaxInterval = CacheBackoffMaxInterval
	pvBackoff.MaxElapsedTime = maxElapsedTime

	if err := backoff.RetryNotify(checkForDeletedPV, pvBackoff, pvNotify); err != nil {
		return fmt.Errorf("PV %s was not fully deleted after %3.2f seconds", name, maxElapsedTime.Seconds())
	}

	return nil
}

// waitForPVCPhase waits for a PVC to reach the specified phase.
func (p *Plugin) waitForPVCPhase(
	pvc *v1.PersistentVolumeClaim, phase v1.PersistentVolumeClaimPhase, maxElapsedTime time.Duration,
) (*v1.PersistentVolumeClaim, error) {

	var latestPVC *v1.PersistentVolumeClaim
	var err error

	checkForPVCPhase := func() error {
		latestPVC, err = p.getCachedPVCByName(pvc.Name, pvc.Namespace)
		if err != nil {
			return err
		} else if latestPVC.Status.Phase != phase {
			return fmt.Errorf("PVC %s/%s not yet %s", pvc.Namespace, pvc.Name, phase)
		}
		return nil
	}
	pvcNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"name":      pvc.Name,
			"namespace": pvc.Namespace,
			"increment": duration,
		}).Debugf("PVC not yet %s, waiting.", phase)
	}
	pvcBackoff := backoff.NewExponentialBackOff()
	pvcBackoff.InitialInterval = CacheBackoffInitialInterval
	pvcBackoff.RandomizationFactor = CacheBackoffRandomizationFactor
	pvcBackoff.Multiplier = CacheBackoffMultiplier
	pvcBackoff.MaxInterval = CacheBackoffMaxInterval
	pvcBackoff.MaxElapsedTime = maxElapsedTime

	if err := backoff.RetryNotify(checkForPVCPhase, pvcBackoff, pvcNotify); err != nil {
		return nil, fmt.Errorf("PVC %s/%s was not %s after %3.2f seconds",
			pvc.Namespace, pvc.Name, phase, maxElapsedTime.Seconds())
	}

	return latestPVC, nil
}

// removePVFinalizers patches a PV by removing all finalizers.
func (p *Plugin) removePVFinalizers(pv *v1.PersistentVolume) (*v1.PersistentVolume, error) {

	pvClone := pv.DeepCopy()
	pvClone.Finalizers = make([]string, 0)
	if patchedPV, err := p.patchPV(pv, pvClone); err != nil {

		message := "could not remove finalizers from PV"
		log.WithFields(log.Fields{
			"PV":    pv.Name,
			"error": err,
		}).Errorf("PV upgrade: %s.", message)
		return nil, fmt.Errorf("%s: %v", message, err)

	} else {
		log.WithField("PV", pv.Name).Info("PV upgrade: removed finalizers from PV.")
		return patchedPV, nil
	}
}

// removePVCBindCompletedAnnotation patches a PVC by removing the bind-completed annotation.
func (p *Plugin) removePVCBindCompletedAnnotation(pvc *v1.PersistentVolumeClaim) (*v1.PersistentVolumeClaim, error) {

	pvcClone := pvc.DeepCopy()
	pvcClone.Annotations = make(map[string]string)

	// Copy all annotations except bind-completed
	if pvc.Annotations != nil {
		for k, v := range pvc.Annotations {
			if k != AnnBindCompleted {
				pvcClone.Annotations[k] = v
			}
		}
	}

	if patchedPVC, err := p.patchPVC(pvc, pvcClone); err != nil {
		return nil, err
	} else {
		return patchedPVC, nil
	}
}

// createCSIPVFromPV accepts an NFS or iSCSI PV plus the corresponding Trident volume, converts the PV
// to a CSI PV, and creates it in Kubernetes.
func (p *Plugin) createCSIPVFromPV(
	pv *v1.PersistentVolume, volume *storage.VolumeExternal,
) (*v1.PersistentVolume, error) {

	fsType := ""
	readOnly := false
	if pv.Spec.NFS != nil {
		readOnly = pv.Spec.NFS.ReadOnly
	} else if pv.Spec.ISCSI != nil {
		readOnly = pv.Spec.ISCSI.ReadOnly
		fsType = pv.Spec.ISCSI.FSType
	}

	volumeAttributes := map[string]string{
		"backendUUID":  volume.BackendUUID,
		"name":         volume.Config.Name,
		"internalName": volume.Config.InternalName,
		"protocol":     string(volume.Config.Protocol),
	}

	csiPV := pv.DeepCopy()
	csiPV.ResourceVersion = ""
	csiPV.UID = ""
	csiPV.Spec.NFS = nil
	csiPV.Spec.ISCSI = nil
	csiPV.Spec.CSI = &v1.CSIPersistentVolumeSource{
		Driver:           csi.Provisioner,
		VolumeHandle:     pv.Name,
		ReadOnly:         readOnly,
		FSType:           fsType,
		VolumeAttributes: volumeAttributes,
	}

	if csiPV.Annotations == nil {
		csiPV.Annotations = make(map[string]string)
	}
	csiPV.Annotations[AnnDynamicallyProvisioned] = csi.Provisioner

	if csiPV, err := p.kubeClient.CoreV1().PersistentVolumes().Create(csiPV); err != nil {
		return nil, err
	} else {
		return csiPV, nil
	}
}

func (p *Plugin) getPodsForPVC(pvc *v1.PersistentVolumeClaim) ([]string, []string, error) {

	nakedPodsForPVC := make([]string, 0)
	ownedPodsForPVC := make([]string, 0)

	podList, err := p.kubeClient.CoreV1().Pods(pvc.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	} else if podList.Items == nil {
		return ownedPodsForPVC, nakedPodsForPVC, nil
	}

	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name {
				if pod.OwnerReferences == nil || len(pod.OwnerReferences) == 0 {
					nakedPodsForPVC = append(nakedPodsForPVC, pod.Name)
				} else {
					ownedPodsForPVC = append(ownedPodsForPVC, pod.Name)
				}
			}
		}
	}

	return ownedPodsForPVC, nakedPodsForPVC, nil
}

// waitForDeletedOrNonRunningPod waits for a pod to be fully deleted or be in a non-Running state.
func (p *Plugin) waitForDeletedOrNonRunningPod(name, namespace string, maxElapsedTime time.Duration) (*v1.Pod, error) {

	var pod *v1.Pod
	var err error

	checkForDeletedPod := func() error {
		if pod, err = p.kubeClient.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{}); err != nil {

			// NotFound is a terminal success condition
			if statusErr, ok := err.(*apierrors.StatusError); ok {
				if statusErr.Status().Reason == metav1.StatusReasonNotFound {
					log.WithField("pod", fmt.Sprintf("%s/%s", namespace, name)).Info("Pod not found.")
					return nil
				}
			}

			// Retry on any other error
			return err

		} else if pod == nil {
			// Shouldn't happen
			return fmt.Errorf("Kubernetes API returned nil for pod %s/%s", namespace, name)
		} else if pod.Status.Phase == v1.PodRunning {
			return fmt.Errorf("pod %s/%s phase is %s", namespace, name, pod.Status.Phase)
		} else {
			// Any phase but Running is a terminal success condition
			log.WithField("pod", fmt.Sprintf("%s/%s", namespace, name)).Infof("Pod phase is %s.", pod.Status.Phase)
			return nil
		}
	}
	podNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"pod":       name,
			"namespace": namespace,
			"increment": duration,
		}).Debugf("Pod not yet deleted, waiting.")
	}
	podBackoff := backoff.NewExponentialBackOff()
	podBackoff.InitialInterval = CacheBackoffInitialInterval
	podBackoff.RandomizationFactor = CacheBackoffRandomizationFactor
	podBackoff.Multiplier = CacheBackoffMultiplier
	podBackoff.MaxInterval = CacheBackoffMaxInterval
	podBackoff.MaxElapsedTime = maxElapsedTime

	if err := backoff.RetryNotify(checkForDeletedPod, podBackoff, podNotify); err != nil {
		return nil, fmt.Errorf("pod %s/%s was not deleted or non-Running after %3.2f seconds",
			namespace, name, maxElapsedTime.Seconds())
	}

	return pod, nil
}