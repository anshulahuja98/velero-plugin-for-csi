/*
Copyright 2020 the Velero contributors.

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

package backup

import (
	"context"
	"fmt"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vmware-tanzu/velero-plugin-for-csi/internal/util"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/label"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

// VolumeSnapshotContentBackupItemAction is a backup item action plugin to backup
// CSI VolumeSnapshotcontent objects using Velero
type VolumeSnapshotContentBackupItemAction struct {
	Log logrus.FieldLogger
}

// AppliesTo returns information indicating that the VolumeSnapshotContentBackupItemAction action should be invoked to backup volumesnapshotcontents.
func (p *VolumeSnapshotContentBackupItemAction) AppliesTo() (velero.ResourceSelector, error) {
	p.Log.Debug("VolumeSnapshotContentBackupItemAction AppliesTo")

	return velero.ResourceSelector{
		IncludedResources: []string{"volumesnapshotcontent.snapshot.storage.k8s.io"},
	}, nil
}

// Execute returns the unmodified volumesnapshotcontent object along with the snapshot deletion secret, if any, from its annotation
// as additional items to backup.
func (p *VolumeSnapshotContentBackupItemAction) Execute(item runtime.Unstructured, backup *velerov1api.Backup) (runtime.Unstructured, []velero.ResourceIdentifier, error) {
	p.Log.Infof("Executing VolumeSnapshotContentBackupItemAction")

	var snapCont snapshotv1api.VolumeSnapshotContent
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.UnstructuredContent(), &snapCont); err != nil {
		return nil, nil, errors.WithStack(err)
	}

	additionalItems := []velero.ResourceIdentifier{}

	// we should backup the snapshot deletion secrets that may be referenced in the volumesnapshotcontent's annotation
	if util.IsVolumeSnapshotContentHasDeleteSecret(&snapCont) {
		// TODO: add GroupResource for secret into kuberesource
		additionalItems = append(additionalItems, velero.ResourceIdentifier{
			GroupResource: schema.GroupResource{Group: "", Resource: "secrets"},
			Name:          snapCont.Annotations[util.PrefixedSnapshotterSecretNameKey],
			Namespace:     snapCont.Annotations[util.PrefixedSnapshotterSecretNamespaceKey],
		})

		util.AddAnnotations(&snapCont.ObjectMeta, map[string]string{
			util.MustIncludeAdditionalItemAnnotation: "true",
		})
	}

	snapContMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&snapCont)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	p.Log.Infof("Returning from VolumeSnapshotContentBackupItemAction with %d additionalItems to backup", len(additionalItems))
	return &unstructured.Unstructured{Object: snapContMap}, additionalItems, nil
}

func (p *VolumeSnapshotContentBackupItemAction) Progress(operationID string, backup *velerov1api.Backup) (velero.OperationProgress, error) {
	progress := velero.OperationProgress{}

	_, snapshotClient, err := util.GetClients()
	if err != nil {
		return progress, errors.WithStack(err)
	}

	// split operation ID to get NS and name of VSC
	_, name, err := util.SplitOperationID(operationID)
	if err != nil {
		return progress, err
	}

	// get VSC
	// snapshotClient.SnapshotV1().VolumeSnapshotContents().Get()
	vsc, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return progress, errors.WithStack(err)
	}

	if vsc != nil {
		// fetch vs using vsc.spec.volumeSnapshotRef .name and .namespace
		vsc, err := snapshotClient.SnapshotV1().VolumeSnapshot().Get(context.TODO(), vsc.Spec.VolumeSnapshotRef.Name, metav1.GetOptions{
		// when we are backing up volumesnapshots created outside of velero, we will not
		// await volumesnapshot reconciliation and in this case GetVolumeSnapshotContentForVolumeSnapshot
		// may not find the associated volumesnapshotcontents to add to the backup.
		// This is not an error encountered in the backup process. So we add the volumesnapshotcontent
		// to the backup only if one is found.
		// additionalItems = append(additionalItems, velero.ResourceIdentifier{
		// 	GroupResource: kuberesource.VolumeSnapshotContents,
		// 	Name:          vsc.Name,
		// })
		annotations[util.CSIVSCDeletionPolicy] = string(vsc.Spec.DeletionPolicy)

		if vsc.Status != nil {
			if vsc.Status.SnapshotHandle != nil {
				// Capture storage provider snapshot handle and CSI driver name
				// to be used on restore to create a static volumesnapshotcontent that will be the source of the volumesnapshot.
				annotations[util.VolumeSnapshotHandleAnnotation] = *vsc.Status.SnapshotHandle
				annotations[util.CSIDriverNameAnnotation] = vsc.Spec.Driver
			}
			if vsc.Status.RestoreSize != nil {
				annotations[util.VolumeSnapshotRestoreSize] = resource.NewQuantity(*vsc.Status.RestoreSize, resource.BinarySI).String()
			}
		}

		if backupOngoing {
			p.Log.Infof("Patching volumensnapshotcontent %s with velero BackupNameLabel", vsc.Name)
			// If we created the volumesnapshotcontent object during this ongoing backup, we would have created it with a DeletionPolicy of Retain.
			// But, we want to retain these volumesnapsshotcontent ONLY for the lifetime of the backup. To that effect, during velero backup
			// deletion, we will update the DeletionPolicy of the volumesnapshotcontent and then delete the VolumeSnapshot object which will
			// cascade delete the volumesnapshotcontent and the associated snapshot in the storage provider (handled by the CSI driver and
			// the CSI common controller).
			// However, in the event that the Volumesnapshot object is deleted outside of the backup deletion process, it is possible that
			// the dynamically created volumesnapshotcontent object will be left as an orphaned and non-discoverable resource in the cluster as well
			// as in the storage provider. To avoid piling up of such orphaned resources, we will want to discover and delete the dynamically created
			// volumesnapshotcontents. We do that by adding the "velero.io/backup-name" label on the volumesnapshotcontent.
			// Further, we want to add this label only on volumesnapshotcontents that were created during an ongoing velero backup.

			pb := []byte(fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`, velerov1api.BackupNameLabel, label.GetValidName(backup.Name)))
			if _, vscPatchError := snapshotClient.SnapshotV1().VolumeSnapshotContents().Patch(context.TODO(), vsc.Name, types.MergePatchType, pb, metav1.PatchOptions{}); vscPatchError != nil {
				p.Log.Warnf("Failed to patch volumesnapshotcontent %s: %v", vsc.Name, vscPatchError)
			}
		}
	}

	// save newly applied annotations into the backed-up volumesnapshot item
	util.AddAnnotations(&vs.ObjectMeta, annotations)

	vsMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&vs)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	return progress, nil
}
