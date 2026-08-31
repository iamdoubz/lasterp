// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// The inbound half of the sandbox (WP-3.2a): the routes a plugin serves under
// `/ext/<id>/`, moved here from WP-3.1b because a plugin-declared route needs
// its own authorization answer and designing that before a caller existed
// would have been guesswork (WP-3.1-decisions.md §8).
//
// The answer, in three parts (WP-3.2-decisions.md §2):
//
//   - The gateway registers **one** route pattern, `/ext/{plugin}/{path...}`,
//     which resolves the plugin from the request's tenant at call time. A mux
//     mutated at install time would be a route table nobody can enumerate, and
//     the route-fence tests exist because enumerating it is how this codebase
//     proves what it exposes.
//   - The **caller** must hold a session and `plugin:invoke` — the same
//     capability that gates calling a plugin function directly, because it is
//     the same power.
//   - The **plugin** still runs as its own principal. The caller's id is passed
//     as context and grants nothing: authority was fixed at install, and an
//     endpoint whose power varied by caller is what WP-3.1a rejected.

// Endpoint response bounds.
const (
	// MaxEndpointResponseBytes caps what a plugin may return through /ext.
	MaxEndpointResponseBytes = 1 << 20
	// MaxEndpointRequestBytes caps what a caller may post to one.
	MaxEndpointRequestBytes = 1 << 20
)

// ErrNoSuchEndpoint means the plugin declares no such path (or not for that
// method). The caller gets a 404 either way: which paths a plugin declares but
// does not serve for this method is not worth a distinct status.
var ErrNoSuchEndpoint = errors.New("plugins: no such endpoint")

// EndpointRequest is what a plugin is told about an inbound call.
//
// Note what is **not** here: the caller's request headers. They carry the
// session cookie and the Authorization header, and handing an untrusted module
// the credentials of the person who called it would make every ext endpoint a
// token-exfiltration surface. A plugin gets the method, the path, the query
// string, the body, and who the caller is — never how they authenticated.
type EndpointRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query"`
	Body   string `json:"body"`
	// Caller is the authenticated user's id, for the plugin's own logic and
	// logging. It is attribution, not authority.
	Caller string `json:"caller"`
}

// EndpointResponse is what the server sends back, after clamping.
type EndpointResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

// pluginReply is the plugin's raw output, before clamping.
type pluginReply struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

// allowedContentTypes is what a plugin may set. **text/html is deliberately
// absent**: a module that could return HTML on the ERP's own origin would have
// scripting rights in the session of whoever opened the link, which is the one
// thing an iframe-less extension surface must not hand out.
var allowedContentTypes = map[string]bool{
	"application/json":          true,
	"text/plain; charset=utf-8": true,
	"text/csv":                  true,
}

// allowedStatuses is the set a plugin may choose from. 3xx is absent (a
// redirect needs a Location header, and no plugin header reaches the wire);
// 401 is absent because only the gateway may issue an authentication
// challenge.
var allowedStatuses = map[int]bool{
	200: true, 201: true, 202: true, 204: true,
	400: true, 403: true, 404: true, 409: true, 422: true, 429: true, 500: true,
}

// ServeEndpoint runs one declared endpoint and returns a clamped response.
func ServeEndpoint(ctx context.Context, h Host, tenant tenancy.ID, p *Installed, req EndpointRequest) (EndpointResponse, error) {
	e, ok := p.Manifest.Endpoint(req.Path)
	if !ok || !e.Serves(req.Method) {
		return EndpointResponse{}, fmt.Errorf("%w: %s %s", ErrNoSuchEndpoint, req.Method, req.Path)
	}
	if len(req.Body) > MaxEndpointRequestBytes {
		return EndpointResponse{}, fmt.Errorf("plugins: request body is over the %d-byte limit", MaxEndpointRequestBytes)
	}
	input, err := json.Marshal(req)
	if err != nil {
		return EndpointResponse{}, err
	}
	out, err := Call(ctx, h, tenant, p, e.Fn, input)
	if err != nil {
		return EndpointResponse{}, err
	}
	return clampReply(out), nil
}

// clampReply turns whatever the plugin returned into a response the server is
// willing to sign its own origin to.
//
// Anything unrecognised becomes a 200 JSON body rather than an error: a plugin
// author's malformed output is their bug to see in the response, and refusing
// the whole call would make debugging an endpoint a matter of reading server
// logs nobody has.
func clampReply(out []byte) EndpointResponse {
	if len(out) > MaxEndpointResponseBytes {
		out = out[:MaxEndpointResponseBytes]
	}
	var reply pluginReply
	if err := json.Unmarshal(out, &reply); err != nil {
		// Not JSON at all: hand it back as plain text rather than pretending.
		return EndpointResponse{Status: http.StatusOK, ContentType: "text/plain; charset=utf-8", Body: out}
	}
	res := EndpointResponse{
		Status:      reply.Status,
		ContentType: strings.ToLower(strings.TrimSpace(reply.ContentType)),
		Body:        []byte(reply.Body),
	}
	if !allowedStatuses[res.Status] {
		res.Status = http.StatusOK
	}
	if !allowedContentTypes[res.ContentType] {
		res.ContentType = "application/json"
	}
	if len(res.Body) > MaxEndpointResponseBytes {
		res.Body = res.Body[:MaxEndpointResponseBytes]
	}
	if res.Status == http.StatusNoContent {
		res.Body = nil
	}
	return res
}

// DeclaredEndpoints renders a plugin's routes for the management API, so an
// administrator can answer "what does this thing expose" from the same place
// they answer "what was it granted".
func (p Installed) DeclaredEndpoints() []map[string]any {
	if p.Manifest == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(p.Manifest.Endpoints))
	for _, e := range p.Manifest.Endpoints {
		out = append(out, map[string]any{
			"path":    "/ext/" + p.ID + e.Path,
			"fn":      e.Fn,
			"methods": e.methods(),
		})
	}
	return out
}

// OutboundHosts renders a plugin's approved outbound allowlist, for the same
// reason.
func (p Installed) OutboundHosts() []map[string]any {
	if p.Manifest == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(p.Manifest.Capabilities.HTTP))
	for _, h := range p.Manifest.Capabilities.HTTP {
		out = append(out, map[string]any{"host": h.Host, "methods": h.methods()})
	}
	return out
}
