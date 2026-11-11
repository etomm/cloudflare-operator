// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The Cloudflare Operator Authors

package dnsrecord

import (
	"context"
	"fmt"
	"strings"

	networkingv1alpha2 "github.com/StringKe/cloudflare-operator/api/v1alpha2"
)

// Multiple DNSRecord resources (usually created by different Ingresses that
// declare the same host) may reference the same Cloudflare DNS record. The
// Cloudflare API identifies records by name+type within a zone, so all of them
// end up adopting the same record ID. The helpers below decide whether another
// DNSRecord still depends on the record before it is deleted.

// normalizedFQDN returns the canonical form of a DNS name used for comparison.
func normalizedFQDN(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// effectiveRecordType returns the DNS record type managed by the resource.
// For dynamic records the type is resolved at runtime and stored in status.
func effectiveRecordType(dnsRecord *networkingv1alpha2.DNSRecord) string {
	if dnsRecord.Spec.Type != "" {
		return strings.ToUpper(dnsRecord.Spec.Type)
	}
	return strings.ToUpper(dnsRecord.Status.ResolvedType)
}

// sameDNSZone reports whether two DNSRecords are expected to manage records in
// the same Cloudflare zone. Explicit zone IDs or domains take precedence; when
// they are absent the records are assumed to share a zone, because a hostname
// can only exist once in Cloudflare.
func sameDNSZone(a, b *networkingv1alpha2.DNSRecord) bool {
	if a.Spec.Cloudflare.ZoneId != "" && b.Spec.Cloudflare.ZoneId != "" &&
		a.Spec.Cloudflare.ZoneId != b.Spec.Cloudflare.ZoneId {
		return false
	}
	aDomain := normalizedFQDN(a.Spec.Cloudflare.Domain)
	bDomain := normalizedFQDN(b.Spec.Cloudflare.Domain)
	return aDomain == "" || bDomain == "" || aDomain == bDomain
}

// claimsSameRecord reports whether other still manages the same logical
// Cloudflare record as dnsRecord.
//
// Records that have already published a record ID are matched by ID: equal IDs
// are direct evidence of shared ownership, different IDs mean the resources
// point at different Cloudflare records. When one side has not synced yet the
// comparison falls back to FQDN, zone and record type.
func claimsSameRecord(dnsRecord, other *networkingv1alpha2.DNSRecord) bool {
	if dnsRecord.Namespace == other.Namespace && dnsRecord.Name == other.Name {
		return false
	}
	if normalizedFQDN(dnsRecord.Spec.Name) != normalizedFQDN(other.Spec.Name) {
		return false
	}
	if !sameDNSZone(dnsRecord, other) {
		return false
	}

	if dnsRecord.Status.RecordID != "" && other.Status.RecordID != "" {
		return dnsRecord.Status.RecordID == other.Status.RecordID
	}

	thisType := effectiveRecordType(dnsRecord)
	otherType := effectiveRecordType(other)
	if thisType != "" && otherType != "" && thisType != otherType {
		return false
	}
	return true
}

// hasOtherLiveOwner returns true when at least one other DNSRecord that is not
// being deleted still claims the same Cloudflare record. Records with a
// deletion timestamp are ignored: they are going away and do not need the
// record anymore, which allows a set of co-owners to be deleted concurrently
// while still deleting the record exactly once.
func (r *DNSRecordReconciler) hasOtherLiveOwner(
	ctx context.Context,
	dnsRecord *networkingv1alpha2.DNSRecord,
) (bool, error) {
	recordList := &networkingv1alpha2.DNSRecordList{}
	if err := r.List(ctx, recordList); err != nil {
		return false, fmt.Errorf("failed to list DNSRecords: %w", err)
	}

	for i := range recordList.Items {
		other := &recordList.Items[i]
		if !other.DeletionTimestamp.IsZero() {
			continue
		}
		if claimsSameRecord(dnsRecord, other) {
			return true, nil
		}
	}
	return false, nil
}
