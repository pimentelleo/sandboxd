package kubernetes

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSnapshotHelperMapsMissingCRD(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "volumesnapshots", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: volumeSnapshotGVR.Group, Resource: volumeSnapshotGVR.Resource}, "workspace-snapshot")
	})
	helper, err := NewSnapshotHelper(client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = helper.Create(context.Background(), "sandboxd-test", SnapshotRequest{
		Name: "workspace-snapshot", SourcePVC: "workspace", ClassName: "csi-snapshots",
	})
	if !errors.Is(err, ErrVolumeSnapshotMissing) {
		t.Fatalf("snapshot create error = %v", err)
	}
}

func TestRestorePVCReferenceUsesVolumeSnapshotDataSource(t *testing.T) {
	reference := RestorePVCReference{Snapshot: VolumeSnapshotRef{Namespace: "sandboxd-test", Name: "snap-a"}}
	source := reference.DataSource()
	if source == nil || source.APIGroup == nil || *source.APIGroup != "snapshot.storage.k8s.io" ||
		source.Kind != "VolumeSnapshot" || source.Name != "snap-a" {
		t.Fatalf("data source = %#v", source)
	}
}
