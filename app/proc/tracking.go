package proc

import (
	"net/url"
	"strings"
)

// trackingParams are query keys that identify the sharer, not the content.
// They make every pasted link unique and poison dedup (#82).
var trackingParams = map[string]bool{
	"si": true, "feature": true, "pp": true,
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"fbclid": true, "gclid": true, "igshid": true, "mc_cid": true, "mc_eid": true,
	"ref": true, "ref_src": true, "cmpid": true,
}

// CleanTrackingParams strips sharer-tracking query keys, keeping everything
// that addresses content (v, t, list, i, …). Unparseable input is returned
// as-is. YouTube ids are not interpreted here.
func CleanTrackingParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return rawURL
	}
	q := u.Query()
	changed := false
	for k := range q {
		if trackingParams[strings.ToLower(k)] {
			q.Del(k)
			changed = true
		}
	}
	if !changed {
		return rawURL
	}
	u.RawQuery = q.Encode()
	return u.String()
}
