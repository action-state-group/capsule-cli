package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file is the only place that knows the mesh node's plugin, its tool
// name and its argument shape. The generic side (evidencedoor.go, profile.go)
// reaches it only through remoteProfileType, validateRemoteConnection and
// openEvidenceDoor.

// remoteProfileType is the profile type that reads a counterparty's book
// through a mesh node's admission-policy plugin.
const remoteProfileType = "mesh-plugin"

// meshEvidenceRequestPath is the host's generic plugin tool route for the
// plugin's evidence-request/1 requester. The host answers 200 with the
// party's answer JSON as the plugin re-encodes it (signatures cover parsed
// fields and hex, so that is lossless for them), or 502 when the party could
// not be reached.
const meshEvidenceRequestPath = "/api/plugins/admission-policy/tools/mesh_evidence_request"

// maxMeshAnswerBytes bounds one answer read from the node. A range answer is
// capped by the responder's page size, so a larger body is not an answer.
const maxMeshAnswerBytes = 16 << 20

// meshRequestTimeout bounds one call, body included. The plugin gives up on a
// silent party after its own idle timeout (8s by default), so this only
// guards a node that accepts the connection and never replies.
const meshRequestTimeout = 60 * time.Second

func validateRemoteConnection(p Profile) error {
	u, e := url.Parse(p.Connection.URL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return inputError(remoteProfileType + " profile needs connection.url: an http(s) base URL with no path, credentials or query")
	}
	hasToken := p.Credentials.Token != Secret{}
	if hasToken && u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return inputError(remoteProfileType + " profile sends its token only over https or to a loopback address")
	}
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// meshPluginDoor carries one evidence-request/1 map to a party and returns
// the answer bytes the node returned, unchanged; it never signs, judges or
// rewrites them.
type meshPluginDoor struct {
	endpoint string
	token    string
	client   *http.Client
}

func openEvidenceDoor(p Profile) (meshPluginDoor, error) {
	if p.Type != remoteProfileType {
		return meshPluginDoor{}, inputError("this profile type cannot read another party's book")
	}
	if e := validateRemoteConnection(p); e != nil {
		return meshPluginDoor{}, e
	}
	token, e := p.Credentials.Token.resolve()
	if e != nil {
		return meshPluginDoor{}, e
	}
	client := &http.Client{
		Timeout: meshRequestTimeout,
		// A redirect would carry the request, and the token, to an address
		// the profile never named.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return meshPluginDoor{endpoint: strings.TrimRight(p.Connection.URL, "/") + meshEvidenceRequestPath, token: token, client: client}, nil
}

// ask sends request, which must already be canonical: the plugin decodes it
// and re-encodes it (serde_json with key order preserved) before the party
// digests it, and canonical bytes come through that round trip unchanged.
func (d meshPluginDoor) ask(ctx context.Context, party string, request []byte) ([]byte, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if e := enc.Encode(struct {
		PeerID  string          `json:"peer_id"`
		Request json.RawMessage `json:"request"`
	}{party, request}); e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, &body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	resp, e := d.client.Do(req)
	if e != nil {
		return nil, errors.Join(ErrNoAnswer, e)
	}
	defer func() { _ = resp.Body.Close() }()
	data, e := io.ReadAll(io.LimitReader(resp.Body, maxMeshAnswerBytes+1))
	if e != nil {
		return nil, errors.Join(ErrNoAnswer, e)
	}
	if resp.StatusCode != http.StatusOK {
		// The host's non-200 body is the plugin's error text; it never
		// carries an answer, and it is not printed (SafeError drops it).
		return nil, errors.Join(ErrNoAnswer, fmt.Errorf("node returned HTTP %d", resp.StatusCode))
	}
	if len(data) > maxMeshAnswerBytes {
		return nil, errors.Join(ErrNoAnswer, errors.New("answer exceeds the size bound"))
	}
	return data, nil
}
