// Package webhook implements the external-dns webhook provider HTTP API.
//
// The JSON types mirror the serialization of the external-dns Go types
// endpoint.Endpoint, plan.Changes and endpoint.DomainFilter, so this module
// does not need to depend on external-dns itself.
// See https://github.com/kubernetes-sigs/external-dns/blob/master/docs/tutorials/webhook-provider.md
package webhook

import "context"

// MediaType is the content type negotiated with external-dns.
const MediaType = "application/external.dns.webhook+json;version=1"

// ProviderSpecificProperty is a provider specific key/value pair of an endpoint.
type ProviderSpecificProperty struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// Endpoint mirrors endpoint.Endpoint.
type Endpoint struct {
	DNSName          string                     `json:"dnsName,omitempty"`
	Targets          []string                   `json:"targets,omitempty"`
	RecordType       string                     `json:"recordType,omitempty"`
	SetIdentifier    string                     `json:"setIdentifier,omitempty"`
	RecordTTL        int64                      `json:"recordTTL,omitempty"`
	Labels           map[string]string          `json:"labels,omitempty"`
	ProviderSpecific []ProviderSpecificProperty `json:"providerSpecific,omitempty"`
}

// Changes mirrors plan.Changes.
type Changes struct {
	Create    []*Endpoint `json:"create,omitempty"`
	UpdateOld []*Endpoint `json:"updateOld,omitempty"`
	UpdateNew []*Endpoint `json:"updateNew,omitempty"`
	Delete    []*Endpoint `json:"delete,omitempty"`
}

// DomainFilter mirrors the JSON form of endpoint.DomainFilter.
type DomainFilter struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// Provider is the DNS backend behind the webhook.
type Provider interface {
	Records(ctx context.Context) ([]*Endpoint, error)
	ApplyChanges(ctx context.Context, changes *Changes) error
	AdjustEndpoints(endpoints []*Endpoint) ([]*Endpoint, error)
	DomainFilter() DomainFilter
}
