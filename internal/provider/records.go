package provider

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// MinTTL and MaxTTL are the TTL limits of the ConnByte API in seconds.
	MinTTL = 60
	MaxTTL = 86400
)

// recordType describes how a DNS record type maps to the ConnByte API.
// The API stores the MX preference and the SRV priority in a separate
// "priority" field; external-dns keeps them as the first token of the target.
type recordType struct {
	hasPriority bool
	hostname    bool
	quoted      bool
}

// recordTypes are the types both external-dns and the ConnByte API support.
var recordTypes = map[string]recordType{
	"A":     {},
	"AAAA":  {},
	"CNAME": {hostname: true},
	"MX":    {hasPriority: true, hostname: true},
	"NS":    {hostname: true},
	"SRV":   {hasPriority: true, hostname: true},
	"TXT":   {quoted: true},
}

// Supported reports whether the record type can be managed by this provider.
func Supported(typ string) bool {
	_, ok := recordTypes[strings.ToUpper(typ)]
	return ok
}

// clampTTL returns the TTL the ConnByte API accepts for the requested TTL.
// A zero TTL means "not configured" and returns def.
func clampTTL(ttl int64, def int) int {
	if ttl <= 0 {
		ttl = int64(def)
	}
	switch {
	case ttl < MinTTL:
		return MinTTL
	case ttl > MaxTTL:
		return MaxTTL
	}
	return int(ttl)
}

// toAPI converts an external-dns target into ConnByte content and priority.
func toAPI(typ, target string) (content string, priority int, err error) {
	rt, ok := recordTypes[typ]
	if !ok {
		return "", 0, fmt.Errorf("record type %q is not supported", typ)
	}
	content = strings.TrimSpace(target)
	if rt.hasPriority {
		prio, rest, found := strings.Cut(content, " ")
		if !found {
			return "", 0, fmt.Errorf("invalid %s target %q: missing priority", typ, target)
		}
		n, convErr := strconv.Atoi(prio)
		if convErr != nil || n < 0 || n > 65535 {
			return "", 0, fmt.Errorf("invalid %s target %q: bad priority", typ, target)
		}
		priority = n
		content = strings.TrimSpace(rest)
	}
	if rt.quoted {
		content = unquote(content)
	}
	if content == "" {
		return "", 0, fmt.Errorf("empty %s target", typ)
	}
	if strings.ContainsAny(content, "\r\n\x00") {
		return "", 0, fmt.Errorf("invalid %s target: control characters are not allowed", typ)
	}
	return content, priority, nil
}

// fromAPI converts ConnByte content and priority into an external-dns target.
func fromAPI(typ, content string, priority int) string {
	rt := recordTypes[typ]
	content = strings.TrimSpace(content)
	if rt.quoted {
		content = unquote(content)
	}
	if rt.hostname {
		content = strings.TrimSuffix(content, ".")
	}
	if rt.hasPriority {
		content = strconv.Itoa(priority) + " " + content
	}
	return content
}

// sameTarget compares two external-dns targets of the same type.
func sameTarget(typ, a, b string) bool {
	return canonical(typ, a) == canonical(typ, b)
}

func canonical(typ, target string) string {
	rt := recordTypes[typ]
	t := strings.TrimSpace(target)
	if rt.quoted {
		return unquote(t)
	}
	if rt.hostname {
		fields := strings.Fields(t)
		for i := range fields {
			fields[i] = strings.ToLower(strings.TrimSuffix(fields[i], "."))
		}
		return strings.Join(fields, " ")
	}
	return strings.ToLower(t)
}

// unquote removes one pair of surrounding double quotes.
// Multi-string TXT values ("a" "b") are left as they are.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' && !strings.Contains(s[1:len(s)-1], `" "`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

// normalizeName lower-cases a DNS name and removes the trailing dot.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// relativeName returns name relative to zone ("" for the apex).
func relativeName(fqdn, zone string) string {
	if fqdn == zone {
		return ""
	}
	return strings.TrimSuffix(fqdn, "."+zone)
}

// inDomain reports whether name equals domain or is a subdomain of it.
func inDomain(name, domain string) bool {
	return name == domain || strings.HasSuffix(name, "."+domain)
}
