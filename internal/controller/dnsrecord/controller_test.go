// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The Cloudflare Operator Authors

package dnsrecord

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha2 "github.com/StringKe/cloudflare-operator/api/v1alpha2"
	"github.com/StringKe/cloudflare-operator/internal/clients/cf"
)

const (
	testZoneID   = "0123456789abcdef0123456789abcdef"
	testFQDN     = "app.example.com"
	testTunnelID = "00000000-0000-0000-0000-000000000000"
)

// fakeDNSAPI is an in-memory Cloudflare DNS API that enforces the same
// name+type uniqueness as Cloudflare (error 81053).
type fakeDNSAPI struct {
	records     map[string]*cf.DNSRecordResult
	nextID      int
	createCalls int
	updateCalls int
	deleteCalls int
}

func newFakeDNSAPI() *fakeDNSAPI {
	return &fakeDNSAPI{records: map[string]*cf.DNSRecordResult{}}
}

func (f *fakeDNSAPI) seed(id, name, recordType, content string) {
	f.records[id] = &cf.DNSRecordResult{
		ID: id, ZoneID: testZoneID, Name: name, Type: recordType, Content: content,
	}
}

func (f *fakeDNSAPI) GetDNSRecordIDInZone(_ context.Context, _, fqdn, recordType string) (string, error) {
	for id, record := range f.records {
		if strings.EqualFold(record.Name, fqdn) && strings.EqualFold(record.Type, recordType) {
			return id, nil
		}
	}
	return "", nil
}

func (f *fakeDNSAPI) CreateDNSRecordInZone(_ context.Context, zoneID string, params cf.DNSRecordParams) (*cf.DNSRecordResult, error) {
	f.createCalls++
	for _, record := range f.records {
		if strings.EqualFold(record.Name, params.Name) && strings.EqualFold(record.Type, params.Type) {
			return nil, fmt.Errorf("An A, AAAA, or CNAME record with that host already exists. (81053)")
		}
	}
	f.nextID++
	id := fmt.Sprintf("%032x", f.nextID)
	f.records[id] = &cf.DNSRecordResult{
		ID: id, ZoneID: zoneID, Name: params.Name, Type: params.Type,
		Content: params.Content, TTL: params.TTL, Proxied: params.Proxied,
	}
	return f.records[id], nil
}

func (f *fakeDNSAPI) UpdateDNSRecordInZone(_ context.Context, zoneID, recordID string, params cf.DNSRecordParams) (*cf.DNSRecordResult, error) {
	f.updateCalls++
	record, ok := f.records[recordID]
	if !ok {
		return nil, fmt.Errorf("Record not found")
	}
	record.Name = params.Name
	record.Type = params.Type
	record.Content = params.Content
	record.TTL = params.TTL
	record.Proxied = params.Proxied
	record.ZoneID = zoneID
	return record, nil
}

func (f *fakeDNSAPI) DeleteDNSRecordInZone(_ context.Context, _, recordID string) error {
	if _, ok := f.records[recordID]; !ok {
		return nil
	}
	delete(f.records, recordID)
	f.deleteCalls++
	return nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, networkingv1alpha2.AddToScheme(scheme))
	return scheme
}

func newTestReconciler(t *testing.T, objs ...client.Object) (*DNSRecordReconciler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&networkingv1alpha2.DNSRecord{}).
		Build()
	return &DNSRecordReconciler{
		Client:   cl,
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(100),
	}, cl
}

// newOwnedRecord returns a DNSRecord that finalizer-protects the given
// Cloudflare record ID.
func newOwnedRecord(name, fqdn, recordID string) *networkingv1alpha2.DNSRecord {
	return &networkingv1alpha2.DNSRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Finalizers: []string{FinalizerName},
		},
		Spec: networkingv1alpha2.DNSRecordSpec{
			Name:    fqdn,
			Type:    "CNAME",
			Content: testTunnelID + ".cfargotunnel.com",
			TTL:     1,
			Proxied: true,
		},
		Status: networkingv1alpha2.DNSRecordStatus{
			RecordID: recordID,
			ZoneID:   testZoneID,
			State:    "Active",
		},
	}
}

func getRecord(t *testing.T, cl client.Client, name string) *networkingv1alpha2.DNSRecord {
	t.Helper()
	record := &networkingv1alpha2.DNSRecord{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, record))
	return record
}

func TestSyncTwoOwnersSameFQDNCreateOneRecord(t *testing.T) {
	ownerA := newOwnedRecord("ingress-a-app-example-com", testFQDN, "")
	ownerB := newOwnedRecord("ingress-b-app-example-com", testFQDN, "")
	ownerA.Finalizers = nil
	ownerB.Finalizers = nil

	r, cl := newTestReconciler(t, ownerA, ownerB)
	api := newFakeDNSAPI()
	ctx := context.Background()

	_, err := r.syncWithAPI(ctx, getRecord(t, cl, ownerA.Name), testZoneID, api, nil)
	require.NoError(t, err)

	_, err = r.syncWithAPI(ctx, getRecord(t, cl, ownerB.Name), testZoneID, api, nil)
	require.NoError(t, err)

	require.Len(t, api.records, 1, "both owners must reference a single Cloudflare record")
	assert.Equal(t, 1, api.createCalls)

	a := getRecord(t, cl, ownerA.Name)
	b := getRecord(t, cl, ownerB.Name)
	assert.NotEmpty(t, a.Status.RecordID)
	assert.Equal(t, a.Status.RecordID, b.Status.RecordID)
	assert.Equal(t, "Active", a.Status.State)
	assert.Equal(t, "Active", b.Status.State)
}

func TestSyncAdoptsRecordOnCreateConflict(t *testing.T) {
	api := newFakeDNSAPI()
	api.seed("11111111111111111111111111111111", testFQDN, "CNAME", testTunnelID+".cfargotunnel.com")

	r, _ := newTestReconciler(t)
	params := cf.DNSRecordParams{
		Name: testFQDN, Type: "CNAME", Content: testTunnelID + ".cfargotunnel.com", TTL: 1, Proxied: true,
	}

	result, err := r.createOrAdoptDNSRecord(context.Background(), testZoneID, params, api)
	require.NoError(t, err)
	assert.Equal(t, "11111111111111111111111111111111", result.ID)
	assert.Equal(t, 1, api.createCalls)
	assert.Equal(t, 1, api.updateCalls)
}

func TestSyncRecreatesMissingRecord(t *testing.T) {
	owner := newOwnedRecord("ingress-a-app-example-com", testFQDN, "22222222222222222222222222222222")
	owner.Finalizers = nil

	r, cl := newTestReconciler(t, owner)
	api := newFakeDNSAPI()

	_, err := r.syncWithAPI(context.Background(), getRecord(t, cl, owner.Name), testZoneID, api, nil)
	require.NoError(t, err)

	updated := getRecord(t, cl, owner.Name)
	require.Len(t, api.records, 1)
	assert.NotEqual(t, "22222222222222222222222222222222", updated.Status.RecordID)
	assert.Contains(t, api.records, updated.Status.RecordID)
	assert.Equal(t, "Active", updated.Status.State)
}

func TestSyncAdoptsRecreatedRecordWithNewID(t *testing.T) {
	owner := newOwnedRecord("ingress-a-app-example-com", testFQDN, "33333333333333333333333333333333")
	owner.Finalizers = nil

	r, cl := newTestReconciler(t, owner)
	api := newFakeDNSAPI()
	api.seed("44444444444444444444444444444444", testFQDN, "CNAME", testTunnelID+".cfargotunnel.com")

	_, err := r.syncWithAPI(context.Background(), getRecord(t, cl, owner.Name), testZoneID, api, nil)
	require.NoError(t, err)

	updated := getRecord(t, cl, owner.Name)
	assert.Equal(t, "44444444444444444444444444444444", updated.Status.RecordID)
	assert.Equal(t, "Active", updated.Status.State)
	require.Len(t, api.records, 1)
}

func TestDeletionKeepsRecordWhenAnotherOwnerRemains(t *testing.T) {
	ownerA := newOwnedRecord("ingress-a-app-example-com", testFQDN, "0123456789abcdef0123456789abcdef")
	ownerB := newOwnedRecord("ingress-b-app-example-com", testFQDN, "0123456789abcdef0123456789abcdef")

	r, cl := newTestReconciler(t, ownerA, ownerB)
	api := newFakeDNSAPI()
	api.seed("0123456789abcdef0123456789abcdef", testFQDN, "CNAME", testTunnelID+".cfargotunnel.com")

	_, err := r.handleDeletionWithAPI(context.Background(), getRecord(t, cl, ownerA.Name), testZoneID, api)
	require.NoError(t, err)

	assert.Equal(t, 0, api.deleteCalls, "shared record must not be deleted")
	assert.Contains(t, api.records, "0123456789abcdef0123456789abcdef")
	assert.NotContains(t, getRecord(t, cl, ownerA.Name).Finalizers, FinalizerName)
}

func TestDeletionRemovesRecordForLastOwner(t *testing.T) {
	owner := newOwnedRecord("ingress-a-app-example-com", testFQDN, "0123456789abcdef0123456789abcdef")

	r, cl := newTestReconciler(t, owner)
	api := newFakeDNSAPI()
	api.seed("0123456789abcdef0123456789abcdef", testFQDN, "CNAME", testTunnelID+".cfargotunnel.com")

	_, err := r.handleDeletionWithAPI(context.Background(), getRecord(t, cl, owner.Name), testZoneID, api)
	require.NoError(t, err)

	assert.Equal(t, 1, api.deleteCalls)
	assert.Empty(t, api.records)
	assert.NotContains(t, getRecord(t, cl, owner.Name).Finalizers, FinalizerName)
}

func TestDeletionIgnoresOwnersBeingDeleted(t *testing.T) {
	ownerA := newOwnedRecord("ingress-a-app-example-com", testFQDN, "0123456789abcdef0123456789abcdef")
	ownerB := newOwnedRecord("ingress-b-app-example-com", testFQDN, "0123456789abcdef0123456789abcdef")
	now := metav1.Now()
	ownerB.DeletionTimestamp = &now

	r, cl := newTestReconciler(t, ownerA, ownerB)
	api := newFakeDNSAPI()
	api.seed("0123456789abcdef0123456789abcdef", testFQDN, "CNAME", testTunnelID+".cfargotunnel.com")

	_, err := r.handleDeletionWithAPI(context.Background(), getRecord(t, cl, ownerA.Name), testZoneID, api)
	require.NoError(t, err)

	assert.Equal(t, 1, api.deleteCalls, "owners that are being deleted no longer need the record")
}

func TestClaimsSameRecord(t *testing.T) {
	cname := newOwnedRecord("a", testFQDN, "55555555555555555555555555555555")

	tests := []struct {
		name  string
		other *networkingv1alpha2.DNSRecord
		want  bool
	}{
		{
			name:  "same record id",
			other: newOwnedRecord("b", testFQDN, "55555555555555555555555555555555"),
			want:  true,
		},
		{
			name:  "different record id",
			other: newOwnedRecord("b", testFQDN, "66666666666666666666666666666666"),
			want:  false,
		},
		{
			name:  "not yet synced same fqdn and type",
			other: newOwnedRecord("b", testFQDN, ""),
			want:  true,
		},
		{
			name:  "different fqdn",
			other: newOwnedRecord("b", "other.example.com", ""),
			want:  false,
		},
		{
			name: "different record type",
			other: func() *networkingv1alpha2.DNSRecord {
				rec := newOwnedRecord("b", testFQDN, "")
				rec.Spec.Type = "TXT"
				return rec
			}(),
			want: false,
		},
		{
			name: "trailing dot and case are normalized",
			other: func() *networkingv1alpha2.DNSRecord {
				rec := newOwnedRecord("b", "APP.Example.COM.", "")
				return rec
			}(),
			want: true,
		},
		{
			name:  "self is not an owner",
			other: newOwnedRecord("a", testFQDN, "55555555555555555555555555555555"),
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, claimsSameRecord(cname, tt.other))
		})
	}
}

func TestClaimsSameRecordDifferentExplicitZone(t *testing.T) {
	zoneA := newOwnedRecord("a", testFQDN, "")
	zoneA.Spec.Cloudflare.ZoneId = "zone-a"
	zoneB := newOwnedRecord("b", testFQDN, "")
	zoneB.Spec.Cloudflare.ZoneId = "zone-b"

	assert.False(t, claimsSameRecord(zoneA, zoneB))
}

// Ensure the fake API satisfies the interface used by the controller.
var _ dnsRecordAPI = (*fakeDNSAPI)(nil)
