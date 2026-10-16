package ops

import (
	"fmt"
	"github.com/jponge/sheaf/internal/bundle"
	"net/url"
	"strings"
)

// ParseRemoteURL returns the host and the "owner/name" slug of a git remote
// URL, as recorded in sheaf.yaml for adopted repositories. Accepted forms:
//
//   - scp-like: [user@]host:owner/name[.git]
//   - URLs: https://, http://, ssh://, git://, file:// (user and port are
//     ignored)
//   - absolute local paths: /path/to/owner/name[.git]
//
// The slug is made of the last two path components, with a trailing ".git"
// removed from the name. For github.com the path must be exactly
// owner/name. For other hosts and local paths (e.g. the file:// origins used
// in tests) the last two components are used whatever the depth, so the slug
// does not necessarily name a GitHub repository. Both parts must only
// contain [A-Za-z0-9._-].
//
// The host is lowercased and has no user or port; file:// URLs and local
// paths have the host bundle.HostLocal ("local"); ssh.github.com and
// www.github.com are github.com. Host aliases from the SSH configuration
// (e.g. "github_work") cannot be resolved here and are returned as-is (adopt
// has --host to override them).
func ParseRemoteURL(raw string) (host, slug string, err error) {
	host, path, err := splitRemote(raw)
	if err != nil {
		return "", "", err
	}
	host = strings.ToLower(host)
	switch host {
	case "":
		host = bundle.HostLocal
	case "ssh.github.com", "www.github.com":
		host = bundle.HostGitHub
	}
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	if len(parts) < 2 {
		return "", "", fmt.Errorf("remote URL %q: cannot derive owner/name from path %q", raw, path)
	}
	if host == bundle.HostGitHub && len(parts) != 2 {
		return "", "", fmt.Errorf("remote URL %q: GitHub path must be owner/name", raw)
	}
	owner, name := parts[len(parts)-2], strings.TrimSuffix(parts[len(parts)-1], ".git")
	if !slugPartOK(owner) || !slugPartOK(name) {
		return "", "", fmt.Errorf("remote URL %q: invalid owner/name %q", raw, owner+"/"+name)
	}
	return host, owner + "/" + name, nil
}

// splitRemote returns the host (empty for local paths) and the path of a
// remote URL.
func splitRemote(raw string) (host, path string, err error) {
	switch {
	case raw == "":
		return "", "", fmt.Errorf("empty remote URL")
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("remote URL %q: %w", raw, err)
		}
		return u.Hostname(), u.Path, nil
	case strings.HasPrefix(raw, "/"):
		return "", raw, nil
	}
	// scp-like syntax: git treats "host:path" as such when the colon comes
	// before any slash.
	before, after, ok := strings.Cut(raw, ":")
	if !ok || strings.Contains(before, "/") {
		return "", "", fmt.Errorf("unsupported remote URL %q", raw)
	}
	if _, h, ok := strings.Cut(before, "@"); ok {
		before = h
	}
	return before, after, nil
}

func slugPartOK(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
