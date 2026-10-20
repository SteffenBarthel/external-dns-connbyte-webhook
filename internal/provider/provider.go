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

// change is one record operation inside a zone. content and priority are the
// API form of target; they are set for creates only.
type change struct {
	zone     zone
	name     string
	typ      string
	target   string
	ttl      int
	content  string
	priority int
}

// rrset collects all changes of one name and type in one zone.
type rrset struct {
	zone    zone
	name    string
	typ     string
	deletes []change
	creates []change
}

type rrKey struct{ zoneID, name, typ string }

// permanentError is a change that will fail again on every retry.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return &permanentError{err: fmt.Errorf(format, args...)}
}

// isPermanent reports whether retrying err on the next sync cannot help:
// invalid input, the domain filter, or an API 4xx answer (except 429).
func isPermanent(err error) bool {
	var pe *permanentError
	if errors.As(err, &pe) {
		return true
	}
	var apiErr *connbyte.APIError
	if errors.As(err, &apiErr) {
		return !apiErr.Temporary()
	}
	return false
}

// ApplyChanges applies the plan, one rrset (name and type) at a time.
//
//   - All new targets are validated first. If one is invalid, the whole update
//     of that name and type is skipped, so the old records stay.
//   - CNAME records are deleted before the new one is created, because a CNAME
//     cannot exist next to other records. If the create fails, the old CNAME
//     is restored.
//   - All other types are created first and deleted afterwards. If a create
//     fails, the old records of that name and type are kept.
//
// Every step is idempotent: records that already exist are not created again
// and records that are gone are not deleted again. Unchanged records inside an
// update are kept. Errors do not stop the run. Temporary errors (network, API
// 5xx or 429) are returned, so external-dns retries. Permanent errors are only
// logged, because they would fail the same way on every sync.
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

	a := &applier{p: p, ctx: ctx, current: map[string][]record{}, created: map[int64]bool{}, deleted: map[int64]bool{}}
	sets := p.plan(zones, changes, &a.errs)

	for _, z := range zones {
		if !touches(z, sets) {
			continue
		}
		recs, err := p.zoneRecords(ctx, z)
		if err != nil {
			a.errs = append(a.errs, fmt.Errorf("list records of zone %s: %w", z.name, err))
			continue
		}
		a.current[z.id] = recs
	}

	for _, s := range sets {
		if _, ok := a.current[s.zone.id]; !ok {
			continue
		}
		if s.typ == "CNAME" {
			a.deleteFirst(s)
		} else {
			a.createFirst(s)
		}
	}
	return p.result(a.errs)
}

// result logs permanent errors and returns the others.
func (p *Provider) result(errs []error) error {
	var retry []error
	for _, err := range errs {
		if isPermanent(err) {
			p.log.Error("change skipped, it is not retried", "error", err)
			continue
		}
		retry = append(retry, err)
	}
	return errors.Join(retry...)
}

// applier holds the state of one ApplyChanges call.
type applier struct {
	p        *Provider
	ctx      context.Context
	current  map[string][]record
	created  map[int64]bool
	deleted  map[int64]bool
	nextFake int64
	errs     []error
}

func (a *applier) createFirst(s *rrset) {
	// A create with the same target as an old record only changes the TTL.
	// It runs after the delete, so the API never sees the same content twice.
	var fresh, ttlOnly []change
	for _, c := range s.creates {
		if sameTargetAs(s.deletes, c) {
			ttlOnly = append(ttlOnly, c)
		} else {
			fresh = append(fresh, c)
		}
	}
	if !a.createAll(fresh) {
		if len(s.deletes) > 0 {
			a.p.log.Warn("keeping old records because a create failed", "zone", s.zone.name, "dnsName", s.name, "recordType", s.typ)
		}
		return
	}
	var removed []record
	for _, d := range s.deletes {
		removed = append(removed, a.delete(d, s.creates)...)
	}
	if !a.createAll(ttlOnly) {
		a.restore(s, removed, ttlOnly)
	}
}

func (a *applier) deleteFirst(s *rrset) {
	var removed []record
	for _, d := range s.deletes {
		removed = append(removed, a.delete(d, s.creates)...)
	}
	if !a.createAll(s.creates) {
		a.restore(s, removed, nil)
	}
}

// createAll creates all changes and reports whether every create succeeded.
func (a *applier) createAll(cs []change) bool {
	ok := true
	for _, c := range cs {
		if err := a.create(c); err != nil {
			a.errs = append(a.errs, err)
			ok = false
		}
	}
	return ok
}

// restore re-creates removed records after a failed create. If only is set,
// only records with a target in only that have no replacement are restored.
func (a *applier) restore(s *rrset, removed []record, only []change) {
	for _, r := range removed {
		if only != nil && !sameTargetAs(only, change{target: r.target}) {
			continue
		}
		if exists(a.current[s.zone.id], a.deleted, change{name: r.name, typ: r.typ, target: r.target, ttl: anyTTL}) {
			continue
		}
		content, prio, err := toAPI(r.typ, r.target)
		if err != nil {
			continue
		}
		old := change{zone: s.zone, name: r.name, typ: r.typ, target: r.target, ttl: r.ttl, content: content, priority: prio}
		a.p.log.Warn("restore old record because the create failed", "zone", s.zone.name, "dnsName", r.name, "recordType", r.typ, "target", r.target)
		if err := a.create(old); err != nil {
			a.errs = append(a.errs, fmt.Errorf("restore %s %s %q: %w", r.typ, r.name, r.target, err))
		}
	}
}

func sameTargetAs(cs []change, c change) bool {
	for _, x := range cs {
		if sameTarget(x.typ, x.target, c.target) {
			return true
		}
	}
	return false
}

// create creates c unless an equal record exists.
func (a *applier) create(c change) error {
	if exists(a.current[c.zone.id], a.deleted, c) {
		return nil
	}
	a.p.log.Info("create record", "zone", c.zone.name, "dnsName", c.name, "recordType", c.typ, "target", c.target, "ttl", c.ttl, "dryRun", a.p.dryRun)
	var id int64
	if a.p.dryRun {
		a.nextFake--
		id = a.nextFake
	} else {
		var err error
		id, err = a.p.api.CreateRecord(a.ctx, connbyte.RecordInput{
			ZoneID:   c.zone.id,
			Name:     relativeName(c.name, c.zone.name),
			Type:     c.typ,
			Content:  c.content,
			TTL:      c.ttl,
			Priority: c.priority,
		})
		if err != nil {
			return fmt.Errorf("create %s %s %q: %w", c.typ, c.name, c.target, err)
		}
	}
	a.created[id] = true
	a.current[c.zone.id] = append(a.current[c.zone.id], record{id: id, name: c.name, typ: c.typ, target: c.target, ttl: c.ttl})
	return nil
}

// delete deletes the records that match d and returns the deleted records.
// Records created in this run and unchanged records that creates still want
// are kept.
func (a *applier) delete(d change, creates []change) []record {
	var out []record
	for _, r := range a.current[d.zone.id] {
		if a.deleted[r.id] || a.created[r.id] || r.name != d.name || r.typ != d.typ || !sameTarget(d.typ, r.target, d.target) {
			continue
		}
		if wantedAgain(creates, r) {
			continue
		}
		a.deleted[r.id] = true
		a.p.log.Info("delete record", "zone", d.zone.name, "dnsName", r.name, "recordType", r.typ, "target", r.target, "id", r.id, "dryRun", a.p.dryRun)
		if !a.p.dryRun {
			if err := a.p.api.DeleteRecord(a.ctx, d.zone.id, r.id); err != nil {
				delete(a.deleted, r.id)
				a.errs = append(a.errs, fmt.Errorf("delete %s %s %q: %w", r.typ, r.name, r.target, err))
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// plan validates the external-dns changes and groups them by name and type.
func (p *Provider) plan(zones []zone, changes *webhook.Changes, errs *[]error) []*rrset {
	type item struct {
		ep     *webhook.Endpoint
		create bool
		update bool
	}
	var items []item
	for _, ep := range changes.Create {
		items = append(items, item{ep: ep, create: true})
	}
	for _, ep := range changes.UpdateNew {
		items = append(items, item{ep: ep, create: true, update: true})
	}
	for _, ep := range changes.UpdateOld {
		items = append(items, item{ep: ep, update: true})
	}
	for _, ep := range changes.Delete {
		items = append(items, item{ep: ep})
	}

	sets := map[rrKey]*rrset{}
	var order []rrKey
	invalid := map[rrKey]bool{}
	type pending struct {
		key    rrKey
		c      change
		create bool
		update bool
	}
	var all []pending

	for _, it := range items {
		if it.ep == nil {
			continue
		}
		name := normalizeName(it.ep.DNSName)
		typ := strings.ToUpper(it.ep.RecordType)
		if !Supported(typ) {
			p.log.Warn("skipping unsupported record type", "dnsName", name, "recordType", it.ep.RecordType)
			continue
		}
		if err := validName(name); err != nil {
			*errs = append(*errs, permanent("%s %q: %w", typ, it.ep.DNSName, err))
			continue
		}
		if !p.nameAllowed(name) {
			*errs = append(*errs, permanent("%s %s is outside the domain filter", typ, name))
			continue
		}
		z, ok := zoneFor(zones, name)
		if !ok {
			*errs = append(*errs, permanent("no ConnByte zone found for %s", name))
			continue
		}
		key := rrKey{z.id, name, typ}
		ttl := 0
		if it.create {
			ttl = clampTTL(it.ep.RecordTTL, p.ttl)
		}
		for _, t := range it.ep.Targets {
			c := change{zone: z, name: name, typ: typ, target: t, ttl: ttl}
			if it.create {
				content, prio, err := toAPI(typ, t)
				if err != nil {
					*errs = append(*errs, permanent("create %s %s: %w", typ, name, err))
					invalid[key] = true
					continue
				}
				c.content, c.priority = content, prio
			}
			all = append(all, pending{key: key, c: c, create: it.create, update: it.update})
		}
	}

	for _, pd := range all {
		if invalid[pd.key] && (pd.create || pd.update) {
			// Skip the whole update of this name and type; the old records stay.
			continue
		}
		s, ok := sets[pd.key]
		if !ok {
			s = &rrset{zone: pd.c.zone, name: pd.c.name, typ: pd.c.typ}
			sets[pd.key] = s
			order = append(order, pd.key)
		}
		if pd.create {
			s.creates = append(s.creates, pd.c)
		} else {
			s.deletes = append(s.deletes, pd.c)
		}
	}

	out := make([]*rrset, 0, len(order))
	for _, k := range order {
		out = append(out, sets[k])
	}
	return out
}

// validName rejects DNS names the ConnByte API must never receive.
func validName(name string) error {
	if name == "" {
		return errors.New("empty DNS name")
	}
	if len(name) > 253 {
		return errors.New("DNS name is longer than 253 characters")
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return errors.New("DNS name contains spaces or control characters")
		}
	}
	return nil
}

func wantedAgain(creates []change, r record) bool {
	for _, c := range creates {
		if c.name == r.name && c.typ == r.typ && c.ttl == r.ttl && sameTarget(c.typ, c.target, r.target) {
			return true
		}
	}
	return false
}

// anyTTL makes exists match a record with any TTL.
const anyTTL = -1

func exists(recs []record, deleted map[int64]bool, c change) bool {
	for _, r := range recs {
		if deleted[r.id] {
			continue
		}
		if r.name == c.name && r.typ == c.typ && (c.ttl == anyTTL || r.ttl == c.ttl) && sameTarget(c.typ, r.target, c.target) {
			return true
		}
	}
	return false
}

func touches(z zone, sets []*rrset) bool {
	for _, s := range sets {
		if s.zone.id == z.id {
			return true
		}
	}
	return false
}
