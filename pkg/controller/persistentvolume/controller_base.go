/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package persistentvolume

import (
	"fmt"
	"strconv"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/client/cache"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	unversioned_core "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/core/unversioned"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/runtime"
	vol "k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/watch"

	"github.com/golang/glog"
)

// This file contains the controller base functionality, i.e. framework to
// process PV/PVC added/updated/deleted events. The real binding, provisioning,
// recycling and deleting is done in controller.go

// NewPersistentVolumeController creates a new PersistentVolumeController
func NewPersistentVolumeController(
	kubeClient clientset.Interface,
	syncPeriod time.Duration,
	provisioner vol.ProvisionableVolumePlugin,
	recyclers []vol.VolumePlugin,
	cloud cloudprovider.Interface,
	clusterName string,
	volumeSource, claimSource cache.ListerWatcher,
	eventRecorder record.EventRecorder,
) *PersistentVolumeController {

	if eventRecorder == nil {
		broadcaster := record.NewBroadcaster()
		broadcaster.StartRecordingToSink(&unversioned_core.EventSinkImpl{Interface: kubeClient.Core().Events("")})
		eventRecorder = broadcaster.NewRecorder(api.EventSource{Component: "persistentvolume-controller"})
	}

	controller := &PersistentVolumeController{
		volumes:                       newPersistentVolumeOrderedIndex(),
		claims:                        cache.NewStore(framework.DeletionHandlingMetaNamespaceKeyFunc),
		kubeClient:                    kubeClient,
		eventRecorder:                 eventRecorder,
		runningOperations:             make(map[string]bool),
		cloud:                         cloud,
		provisioner:                   provisioner,
		clusterName:                   clusterName,
		createProvisionedPVRetryCount: createProvisionedPVRetryCount,
		createProvisionedPVInterval:   createProvisionedPVInterval,
	}

	controller.recyclePluginMgr.InitPlugins(recyclers, controller)
	if controller.provisioner != nil {
		if err := controller.provisioner.Init(controller); err != nil {
			glog.Errorf("PersistentVolumeController: error initializing provisioner plugin: %v", err)
		}
	}

	if volumeSource == nil {
		volumeSource = &cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return kubeClient.Core().PersistentVolumes().List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return kubeClient.Core().PersistentVolumes().Watch(options)
			},
		}
	}

	if claimSource == nil {
		claimSource = &cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return kubeClient.Core().PersistentVolumeClaims(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return kubeClient.Core().PersistentVolumeClaims(api.NamespaceAll).Watch(options)
			},
		}
	}

	_, controller.volumeController = framework.NewIndexerInformer(
		volumeSource,
		&api.PersistentVolume{},
		syncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    controller.addVolume,
			UpdateFunc: controller.updateVolume,
			DeleteFunc: controller.deleteVolume,
		},
		cache.Indexers{"accessmodes": accessModesIndexFunc},
	)
	_, controller.claimController = framework.NewInformer(
		claimSource,
		&api.PersistentVolumeClaim{},
		syncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    controller.addClaim,
			UpdateFunc: controller.updateClaim,
			DeleteFunc: controller.deleteClaim,
		},
	)
	return controller
}

// addVolume is callback from framework.Controller watching PersistentVolume
// events.
func (ctrl *PersistentVolumeController) addVolume(obj interface{}) {
	// Store the new volume version in the cache and do not process it if this
	// is an old version.
	new, err := storeObjectUpdate(ctrl.volumes.store, obj, "volume")
	if err != nil {
		glog.Errorf("%v", err)
	}
	if !new {
		return
	}

	if !ctrl.isFullySynced() {
		return
	}

	pv, ok := obj.(*api.PersistentVolume)
	if !ok {
		glog.Errorf("expected PersistentVolume but handler received %+v", obj)
		return
	}
	if err := ctrl.syncVolume(pv); err != nil {
		if errors.IsConflict(err) {
			// Version conflict error happens quite often and the controller
			// recovers from it easily.
			glog.V(3).Infof("PersistentVolumeController could not add volume %q: %+v", pv.Name, err)
		} else {
			glog.Errorf("PersistentVolumeController could not add volume %q: %+v", pv.Name, err)
		}
	}
}

// updateVolume is callback from framework.Controller watching PersistentVolume
// events.
func (ctrl *PersistentVolumeController) updateVolume(oldObj, newObj interface{}) {
	// Store the new volume version in the cache and do not process it if this
	// is an old version.
	new, err := storeObjectUpdate(ctrl.volumes.store, newObj, "volume")
	if err != nil {
		glog.Errorf("%v", err)
	}
	if !new {
		return
	}

	if !ctrl.isFullySynced() {
		return
	}

	newVolume, ok := newObj.(*api.PersistentVolume)
	if !ok {
		glog.Errorf("Expected PersistentVolume but handler received %+v", newObj)
		return
	}
	if err := ctrl.syncVolume(newVolume); err != nil {
		if errors.IsConflict(err) {
			// Version conflict error happens quite often and the controller
			// recovers from it easily.
			glog.V(3).Infof("PersistentVolumeController could not update volume %q: %+v", newVolume.Name, err)
		} else {
			glog.Errorf("PersistentVolumeController could not update volume %q: %+v", newVolume.Name, err)
		}
	}
}

// deleteVolume is callback from framework.Controller watching PersistentVolume
// events.
func (ctrl *PersistentVolumeController) deleteVolume(obj interface{}) {
	_ = ctrl.volumes.store.Delete(obj)

	if !ctrl.isFullySynced() {
		return
	}

	var volume *api.PersistentVolume
	var ok bool
	volume, ok = obj.(*api.PersistentVolume)
	if !ok {
		if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
			volume, ok = unknown.Obj.(*api.PersistentVolume)
			if !ok {
				glog.Errorf("Expected PersistentVolume but deleteVolume received %+v", unknown.Obj)
				return
			}
		} else {
			glog.Errorf("Expected PersistentVolume but deleteVolume received %+v", obj)
			return
		}
	}

	if !ok || volume == nil || volume.Spec.ClaimRef == nil {
		return
	}

	if claimObj, exists, _ := ctrl.claims.GetByKey(claimrefToClaimKey(volume.Spec.ClaimRef)); exists {
		if claim, ok := claimObj.(*api.PersistentVolumeClaim); ok && claim != nil {
			// sync the claim when its volume is deleted. Explicitly syncing the
			// claim here in response to volume deletion prevents the claim from
			// waiting until the next sync period for its Lost status.
			err := ctrl.syncClaim(claim)
			if err != nil {
				if errors.IsConflict(err) {
					// Version conflict error happens quite often and the
					// controller recovers from it easily.
					glog.V(3).Infof("PersistentVolumeController could not update volume %q from deleteVolume handler: %+v", claimToClaimKey(claim), err)
				} else {
					glog.Errorf("PersistentVolumeController could not update volume %q from deleteVolume handler: %+v", claimToClaimKey(claim), err)
				}
			}
		} else {
			glog.Errorf("Cannot convert object from claim cache to claim %q!?: %+v", claimrefToClaimKey(volume.Spec.ClaimRef), claimObj)
		}
	}
}

// addClaim is callback from framework.Controller watching PersistentVolumeClaim
// events.
func (ctrl *PersistentVolumeController) addClaim(obj interface{}) {
	// Store the new claim version in the cache and do not process it if this is
	// an old version.
	new, err := storeObjectUpdate(ctrl.claims, obj, "claim")
	if err != nil {
		glog.Errorf("%v", err)
	}
	if !new {
		return
	}

	if !ctrl.isFullySynced() {
		return
	}

	claim, ok := obj.(*api.PersistentVolumeClaim)
	if !ok {
		glog.Errorf("Expected PersistentVolumeClaim but addClaim received %+v", obj)
		return
	}
	if err := ctrl.syncClaim(claim); err != nil {
		if errors.IsConflict(err) {
			// Version conflict error happens quite often and the controller
			// recovers from it easily.
			glog.V(3).Infof("PersistentVolumeController could not add claim %q: %+v", claimToClaimKey(claim), err)
		} else {
			glog.Errorf("PersistentVolumeController could not add claim %q: %+v", claimToClaimKey(claim), err)
		}
	}
}

// updateClaim is callback from framework.Controller watching PersistentVolumeClaim
// events.
func (ctrl *PersistentVolumeController) updateClaim(oldObj, newObj interface{}) {
	// Store the new claim version in the cache and do not process it if this is
	// an old version.
	new, err := storeObjectUpdate(ctrl.claims, newObj, "claim")
	if err != nil {
		glog.Errorf("%v", err)
	}
	if !new {
		return
	}

	if !ctrl.isFullySynced() {
		return
	}

	newClaim, ok := newObj.(*api.PersistentVolumeClaim)
	if !ok {
		glog.Errorf("Expected PersistentVolumeClaim but updateClaim received %+v", newObj)
		return
	}
	if err := ctrl.syncClaim(newClaim); err != nil {
		if errors.IsConflict(err) {
			// Version conflict error happens quite often and the controller
			// recovers from it easily.
			glog.V(3).Infof("PersistentVolumeController could not update claim %q: %+v", claimToClaimKey(newClaim), err)
		} else {
			glog.Errorf("PersistentVolumeController could not update claim %q: %+v", claimToClaimKey(newClaim), err)
		}
	}
}

// deleteClaim is callback from framework.Controller watching PersistentVolumeClaim
// events.
func (ctrl *PersistentVolumeController) deleteClaim(obj interface{}) {
	_ = ctrl.claims.Delete(obj)

	if !ctrl.isFullySynced() {
		return
	}

	var volume *api.PersistentVolume
	var claim *api.PersistentVolumeClaim
	var ok bool

	claim, ok = obj.(*api.PersistentVolumeClaim)
	if !ok {
		if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
			claim, ok = unknown.Obj.(*api.PersistentVolumeClaim)
			if !ok {
				glog.Errorf("Expected PersistentVolumeClaim but deleteClaim received %+v", unknown.Obj)
				return
			}
		} else {
			glog.Errorf("Expected PersistentVolumeClaim but deleteClaim received %+v", obj)
			return
		}
	}

	if !ok || claim == nil {
		return
	}

	if pvObj, exists, _ := ctrl.volumes.store.GetByKey(claim.Spec.VolumeName); exists {
		if volume, ok = pvObj.(*api.PersistentVolume); ok {
			// sync the volume when its claim is deleted.  Explicitly sync'ing the
			// volume here in response to claim deletion prevents the volume from
			// waiting until the next sync period for its Release.
			if volume != nil {
				err := ctrl.syncVolume(volume)
				if err != nil {
					if errors.IsConflict(err) {
						// Version conflict error happens quite often and the
						// controller recovers from it easily.
						glog.V(3).Infof("PersistentVolumeController could not update volume %q from deleteClaim handler: %+v", volume.Name, err)
					} else {
						glog.Errorf("PersistentVolumeController could not update volume %q from deleteClaim handler: %+v", volume.Name, err)
					}
				}
			}
		} else {
			glog.Errorf("Cannot convert object from volume cache to volume %q!?: %+v", claim.Spec.VolumeName, pvObj)
		}
	}
}

// Run starts all of this controller's control loops
func (ctrl *PersistentVolumeController) Run() {
	glog.V(4).Infof("starting PersistentVolumeController")

	if ctrl.volumeControllerStopCh == nil {
		ctrl.volumeControllerStopCh = make(chan struct{})
		go ctrl.volumeController.Run(ctrl.volumeControllerStopCh)
	}

	if ctrl.claimControllerStopCh == nil {
		ctrl.claimControllerStopCh = make(chan struct{})
		go ctrl.claimController.Run(ctrl.claimControllerStopCh)
	}
}

// Stop gracefully shuts down this controller
func (ctrl *PersistentVolumeController) Stop() {
	glog.V(4).Infof("stopping PersistentVolumeController")
	close(ctrl.volumeControllerStopCh)
	close(ctrl.claimControllerStopCh)
}

// isFullySynced returns true, if both volume and claim caches are fully loaded
// after startup.
// We do not want to process events with not fully loaded caches - e.g. we might
// recycle/delete PVs that don't have corresponding claim in the cache yet.
func (ctrl *PersistentVolumeController) isFullySynced() bool {
	return ctrl.volumeController.HasSynced() && ctrl.claimController.HasSynced()
}

// Stateless functions

func hasAnnotation(obj api.ObjectMeta, ann string) bool {
	_, found := obj.Annotations[ann]
	return found
}

func setAnnotation(obj *api.ObjectMeta, ann string, value string) {
	if obj.Annotations == nil {
		obj.Annotations = make(map[string]string)
	}
	obj.Annotations[ann] = value
}

func getClaimStatusForLogging(claim *api.PersistentVolumeClaim) string {
	bound := hasAnnotation(claim.ObjectMeta, annBindCompleted)
	boundByController := hasAnnotation(claim.ObjectMeta, annBoundByController)

	return fmt.Sprintf("phase: %s, bound to: %q, bindCompleted: %v, boundByController: %v", claim.Status.Phase, claim.Spec.VolumeName, bound, boundByController)
}

func getVolumeStatusForLogging(volume *api.PersistentVolume) string {
	boundByController := hasAnnotation(volume.ObjectMeta, annBoundByController)
	claimName := ""
	if volume.Spec.ClaimRef != nil {
		claimName = fmt.Sprintf("%s/%s (uid: %s)", volume.Spec.ClaimRef.Namespace, volume.Spec.ClaimRef.Name, volume.Spec.ClaimRef.UID)
	}
	return fmt.Sprintf("phase: %s, bound to: %q, boundByController: %v", volume.Status.Phase, claimName, boundByController)
}

// isVolumeBoundToClaim returns true, if given volume is pre-bound or bound
// to specific claim. Both claim.Name and claim.Namespace must be equal.
// If claim.UID is present in volume.Spec.ClaimRef, it must be equal too.
func isVolumeBoundToClaim(volume *api.PersistentVolume, claim *api.PersistentVolumeClaim) bool {
	if volume.Spec.ClaimRef == nil {
		return false
	}
	if claim.Name != volume.Spec.ClaimRef.Name || claim.Namespace != volume.Spec.ClaimRef.Namespace {
		return false
	}
	if volume.Spec.ClaimRef.UID != "" && claim.UID != volume.Spec.ClaimRef.UID {
		return false
	}
	return true
}

// storeObjectUpdate updates given cache with a new object version from Informer
// callback (i.e. with events from etcd) or with an object modified by the
// controller itself. Returns "true", if the cache was updated, false if the
// object is an old version and should be ignored.
func storeObjectUpdate(store cache.Store, obj interface{}, className string) (bool, error) {
	objAccessor, err := meta.Accessor(obj)
	if err != nil {
		return false, fmt.Errorf("Error reading cache of %s: %v", className, err)
	}
	objName := objAccessor.GetNamespace() + "/" + objAccessor.GetName()

	oldObj, found, err := store.Get(obj)
	if err != nil {
		return false, fmt.Errorf("Error finding %s %q in controller cache: %v", className, objName, err)
	}

	if !found {
		// This is a new object
		glog.V(4).Infof("storeObjectUpdate: adding %s %q, version %s", className, objName, objAccessor.GetResourceVersion())
		if err = store.Add(obj); err != nil {
			return false, fmt.Errorf("Error adding %s %q to controller cache: %v", className, objName, err)
		}
		return true, nil
	}

	oldObjAccessor, err := meta.Accessor(oldObj)
	if err != nil {
		return false, err
	}

	objResourceVersion, err := strconv.ParseInt(objAccessor.GetResourceVersion(), 10, 64)
	if err != nil {
		return false, fmt.Errorf("Error parsing ResourceVersion %q of %s %q: %s", objAccessor.GetResourceVersion(), className, objName, err)
	}
	oldObjResourceVersion, err := strconv.ParseInt(oldObjAccessor.GetResourceVersion(), 10, 64)
	if err != nil {
		return false, fmt.Errorf("Error parsing old ResourceVersion %q of %s %q: %s", oldObjAccessor.GetResourceVersion(), className, objName, err)
	}

	// Throw away only older version, let the same version pass - we do want to
	// get periodic sync events.
	if oldObjResourceVersion > objResourceVersion {
		glog.V(4).Infof("storeObjectUpdate: ignoring %s %q version %s", className, objName, objAccessor.GetResourceVersion())
		return false, nil
	}

	glog.V(4).Infof("storeObjectUpdate updating %s %q with version %s", className, objName, objAccessor.GetResourceVersion())
	if err = store.Update(obj); err != nil {
		return false, fmt.Errorf("Error updating %s %q in controller cache: %v", className, objName, err)
	}
	return true, nil
}
