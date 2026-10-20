// Package provider maps external-dns endpoints to ConnByte DNS records.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/connbyte"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/webhook"
)

// API is the subset of the ConnByte client used by the provider.
type API interface {
	ListZones(ctx context.Context) ([]connbyte.Zone, error)
	ListRecords(ctx context.Context, zoneID string) ([]connbyte.Record, error)
	CreateRecord(ctx context.Context, in connbyte.RecordInput) (int64, error)
	DeleteRecord(ctx context.Context, zoneID string, recordID int64) error
}

// Config configures the provider.
type Config struct {
	// DomainFilter limits the zones and names the provider manages.
	// Empty means all zones of the account.
	DomainFilter []string
	// ExcludeDomains are never touched, even if they match DomainFilter.
	ExcludeDomains []string
	// DefaultTTL is used for endpoints without a TTL (seconds).
	DefaultTTL int
	// DryRun logs changes instead of applying them.
	DryRun bool
}

// Provider implements webhook.Provider for ConnByte.
type Provider struct {
	api     API
	log     *slog.Logger
	include []string
	exclude []string
	ttl     int
	dryRun  bool
	mu      sync.Mutex
}

var _ webhook.Provider = (*Provider)(nil)

// New returns a provider for the given API.
func New(api API, cfg Config, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	ttl := cfg.DefaultTTL
	if ttl <= 0 {
		ttl = 300
	}
	return &Provider{
		api:     api,
		log:     log,
		include: normalizeList(cfg.DomainFilter),
		exclude: normalizeList(cfg.ExcludeDomains),
		ttl:     clampTTL(int64(ttl), ttl),
		dryRun:  cfg.DryRun,
	}
}

func normalizeList(in []string) []string {
	var out []string
	for _, s := range in {
		for _, part := range strings.Split(s, ",") {
			if n := normalizeName(part); n != "" {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// DomainFilter returns the domain filter negotiated with external-dns.
func (p *Provider) DomainFilter() webhook.DomainFilter {
	return webhook.DomainFilter{Include: p.include, Exclude: p.exclude}
}

// nameAllowed reports whether a DNS name may be managed.
func (p *Provider) nameAllowed(name string) bool {
	for _, ex := range p.exclude {
		if inDomain(name, ex) {
			return false
		}
	}
	if len(p.include) == 0 {
		return true
	}
	for _, in := range p.include {
		if inDomain(name, in) {
			return true
		}
	}
	return false
}

// zoneAllowed reports whether a zone can contain names the provider manages.
func (p *Provider) zoneAllowed(zone string) bool {
	if len(p.include) == 0 {
		return !p.zoneExcluded(zone)
	}
	for _, in := range p.include {
		if inDomain(zone, in) || inDomain(in, zone) {
			return !p.zoneExcluded(zone)
		}
	}
	return false
}

func (p *Provider) zoneExcluded(zone string) bool {
	for _, ex := range p.exclude {
		if inDomain(zone, ex) {
			return true
		}
	}
	return false
}

type zone struct {
	id   string
	name string
}

func (p *Provider) zones(ctx context.Context) ([]zone, error) {
	all, err := p.api.ListZones(ctx)
	if err != nil {
		return nil, err
	}
	var out []zone
	for _, z := range all {
		name := normalizeName(z.Zone)
		if name == "" || z.ID == "" || !p.zoneAllowed(name) {
			continue
		}
		out = append(out, zone{id: z.ID, name: name})
	}
	// Longest zone first, so the most specific zone wins.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].name) != len(out[j].name) {
			return len(out[i].name) > len(out[j].name)
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

// zoneFor returns the most specific zone for a DNS name.
func zoneFor(zones []zone, name string) (zone, bool) {
	for _, z := range zones {
		if inDomain(name, z.name) {
			return z, true
		}
	}
	return zone{}, false
}

// record is a ConnByte record converted to external-dns terms.
type record struct {
	id     int64
	name   string
	typ    string
	target string
	ttl    int
}

// recordName returns the FQDN of an API record in zone z.
// The API returns FQDNs; relative names are accepted as well.
func recordName(apiName, zoneName string) string {
	n := normalizeName(apiName)
	if n == "" || n == "@" {
		return zoneName
	}
	if inDomain(n, zoneName) {
		return n
	}
	return n + "." + zoneName
}

func (p *Provider) zoneRecords(ctx context.Context, z zone) ([]record, error) {
	recs, err := p.api.ListRecords(ctx, z.id)
	if err != nil {
		return nil, err
	}
	out := make([]record, 0, len(recs))
	for _, r := range recs {
		typ := strings.ToUpper(r.Type)
		if !Supported(typ) || r.IsDisabled() {
			continue
		}
		out = append(out, record{
			id:     r.ID,
			name:   recordName(r.Name, z.name),
			typ:    typ,
			target: fromAPI(typ, r.Content, r.Priority),
			ttl:    r.TTL,
		})
	}
	return out, nil
}

// Records returns the current records of all managed zones as endpoints.
func (p *Provider) Records(ctx context.Context) ([]*webhook.Endpoint, error) {
	zones, err := p.zones(ctx)
	if err != nil {
		return nil, err
	}
	type key struct{ name, typ string }
	byKey := map[key]*webhook.Endpoint{}
	var order []key
	for _, z := range zones {
		recs, err := p.zoneRecords(ctx, z)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if !p.nameAllowed(r.name) {
				continue
			}
			// A name that belongs to a more specific managed zone is reported there.
			if best, ok := zoneFor(zones, r.name); !ok || best.id != z.id {
				continue
			}
			k := key{r.name, r.typ}
			ep, ok := byKey[k]
			if !ok {
				ep = &webhook.Endpoint{DNSName: r.name, RecordType: r.typ, RecordTTL: int64(r.ttl)}
				byKey[k] = ep
				order = append(order, k)
			}
			if int64(r.ttl) < ep.RecordTTL {
				ep.RecordTTL = int64(r.ttl)
			}
			ep.Targets = append(ep.Targets, r.target)
		}
	}
	out := make([]*webhook.Endpoint, 0, len(order))
	for _, k := range order {
		ep := byKey[k]
		sort.Strings(ep.Targets)
		out = append(out, ep)
	}
	return out, nil
}

// AdjustEndpoints drops unsupported types and normalizes TTLs and targets the
// way the ConnByte API stores them, so external-dns does not plan updates forever.
func (p *Provider) AdjustEndpoints(eps []*webhook.Endpoint) ([]*webhook.Endpoint, error) {
	out := make([]*webhook.Endpoint, 0, len(eps))
	for _, ep := range eps {
		if ep == nil {
			continue
		}
		typ := strings.ToUpper(ep.RecordType)
		if !Supported(typ) {
			p.log.Warn("skipping unsupported record type", "dnsName", ep.DNSName, "recordType", ep.RecordType)
			continue
		}
		ep.RecordType = typ
		ep.DNSName = normalizeName(ep.DNSName)
		if ep.RecordTTL > 0 {
			ep.RecordTTL = int64(clampTTL(ep.RecordTTL, p.ttl))
		}
		for i, t := range ep.Targets {
			ep.Targets[i] = fromAPIForm(typ, t)
		}
		out = append(out, ep)
	}
	return out, nil
}

// fromAPIForm returns the target in the form Records reports it.
func fromAPIForm(typ, target string) string {
	content, prio, err := toAPI(typ, target)
	if err != nil {
		return target
	}
	return fromAPI(typ, content, prio)
}

// change is one record operation inside a zone.
type change struct {
	zone   zone
	name   string
	typ    string
	target string
	ttl    int
}

// ApplyChanges applies the plan. Deletes run before creates, so updates that
// change a target or TTL work. Every step is idempotent: records that already
// exist are not created again and records that are gone are not deleted again.
// Errors do not stop the run; all errors are returned together and
// external-dns retries on the next sync.
func (p *Provider) ApplyChanges(ctx context.Context, changes *webhook.Changes) error {
	if changes == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	zones, err := p.zones(ctx)
	if err != nil {
		return err
	}

	var errs []error
	deletes, creates := p.plan(zones, changes, &errs)
	if len(deletes) == 0 && len(creates) == 0 {
		return errors.Join(errs...)
	}

	current := map[string][]record{}
	for _, z := range zones {
		if !touches(z, deletes, creates) {
			continue
		}
		recs, err := p.zoneRecords(ctx, z)
		if err != nil {
			errs = append(errs, fmt.Errorf("list records of zone %s: %w", z.name, err))
			continue
		}
		current[z.id] = recs
	}

	deleted := map[int64]bool{}
	for _, d := range deletes {
		recs, ok := current[d.zone.id]
		if !ok {
			continue
		}
		for _, r := range recs {
			if deleted[r.id] || r.name != d.name || r.typ != d.typ || !sameTarget(d.typ, r.target, d.target) {
				continue
			}
			if d.ttl > 0 && r.ttl == d.ttl && wantedAgain(creates, r) {
				// Unchanged record inside an update: keep it.
				continue
			}
			deleted[r.id] = true
			p.log.Info("delete record", "zone", d.zone.name, "dnsName", r.name, "recordType", r.typ, "target", r.target, "id", r.id, "dryRun", p.dryRun)
			if p.dryRun {
				continue
			}
			if err := p.api.DeleteRecord(ctx, d.zone.id, r.id); err != nil {
				errs = append(errs, fmt.Errorf("delete %s %s %q: %w", r.typ, r.name, r.target, err))
			}
		}
	}

	for _, c := range creates {
		recs, ok := current[c.zone.id]
		if !ok {
			continue
		}
		if exists(recs, deleted, c) {
			continue
		}
		content, prio, err := toAPI(c.typ, c.target)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s %s: %w", c.typ, c.name, err))
			continue
		}
		in := connbyte.RecordInput{
			ZoneID:   c.zone.id,
			Name:     relativeName(c.name, c.zone.name),
			Type:     c.typ,
			Content:  content,
			TTL:      c.ttl,
			Priority: prio,
		}
		p.log.Info("create record", "zone", c.zone.name, "dnsName", c.name, "recordType", c.typ, "target", c.target, "ttl", c.ttl, "dryRun", p.dryRun)
		if p.dryRun {
			continue
		}
		id, err := p.api.CreateRecord(ctx, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s %s %q: %w", c.typ, c.name, c.target, err))
			continue
		}
		current[c.zone.id] = append(current[c.zone.id], record{id: id, name: c.name, typ: c.typ, target: c.target, ttl: c.ttl})
	}

	return errors.Join(errs...)
}

// plan turns the external-dns changes into record deletes and creates.
func (p *Provider) plan(zones []zone, changes *webhook.Changes, errs *[]error) (deletes, creates []change) {
	expand := func(eps []*webhook.Endpoint, withTTL bool) []change {
		var out []change
		for _, ep := range eps {
			if ep == nil {
				continue
			}
			name := normalizeName(ep.DNSName)
			typ := strings.ToUpper(ep.RecordType)
			if !Supported(typ) {
				p.log.Warn("skipping unsupported record type", "dnsName", name, "recordType", ep.RecordType)
				continue
			}
			if !p.nameAllowed(name) {
				*errs = append(*errs, fmt.Errorf("%s %s is outside the domain filter", typ, name))
				continue
			}
			z, ok := zoneFor(zones, name)
			if !ok {
				*errs = append(*errs, fmt.Errorf("no ConnByte zone found for %s", name))
				continue
			}
			ttl := 0
			if withTTL {
				ttl = clampTTL(ep.RecordTTL, p.ttl)
			}
			for _, t := range ep.Targets {
				out = append(out, change{zone: z, name: name, typ: typ, target: t, ttl: ttl})
			}
		}
		return out
	}

	creates = append(expand(changes.Create, true), expand(changes.UpdateNew, true)...)
	deletes = append(expand(changes.Delete, false), p.updateDeletes(expand(changes.UpdateOld, false), creates)...)
	return deletes, creates
}

// updateDeletes marks old update targets; ttl>0 tells ApplyChanges to keep a
// record that is unchanged in the new endpoint.
func (p *Provider) updateDeletes(old, creates []change) []change {
	for i := range old {
		for _, c := range creates {
			if c.zone.id == old[i].zone.id && c.name == old[i].name && c.typ == old[i].typ && sameTarget(c.typ, c.target, old[i].target) {
				old[i].ttl = c.ttl
				break
			}
		}
	}
	return old
}

func wantedAgain(creates []change, r record) bool {
	for _, c := range creates {
		if c.name == r.name && c.typ == r.typ && c.ttl == r.ttl && sameTarget(c.typ, c.target, r.target) {
			return true
		}
	}
	return false
}

func exists(recs []record, deleted map[int64]bool, c change) bool {
	for _, r := range recs {
		if deleted[r.id] {
			continue
		}
		if r.name == c.name && r.typ == c.typ && r.ttl == c.ttl && sameTarget(c.typ, r.target, c.target) {
			return true
		}
	}
	return false
}

func touches(z zone, lists ...[]change) bool {
	for _, l := range lists {
		for _, c := range l {
			if c.zone.id == z.id {
				return true
			}
		}
	}
	return false
}
