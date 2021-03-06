package vault

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/armon/go-radix"
	"github.com/hashicorp/vault/logical"
)

// Router is used to do prefix based routing of a request to a logical backend
type Router struct {
	l    sync.RWMutex
	root *radix.Tree
}

// NewRouter returns a new router
func NewRouter() *Router {
	r := &Router{
		root: radix.New(),
	}
	return r
}

// mountEntry is used to represent a mount point
type mountEntry struct {
	tainted    bool
	salt       string
	backend    logical.Backend
	view       *BarrierView
	rootPaths  *radix.Tree
	loginPaths *radix.Tree
}

// SaltID is used to apply a salt and hash to an ID to make sure its not reversable
func (me *mountEntry) SaltID(id string) string {
	comb := me.salt + id
	hash := sha1.Sum([]byte(comb))
	return hex.EncodeToString(hash[:])
}

// Mount is used to expose a logical backend at a given prefix, using a unique salt,
// and the barrier view for that path.
func (r *Router) Mount(backend logical.Backend, prefix, salt string, view *BarrierView) error {
	r.l.Lock()
	defer r.l.Unlock()

	// Check if this is a nested mount
	if existing, _, ok := r.root.LongestPrefix(prefix); ok && existing != "" {
		return fmt.Errorf("cannot mount under existing mount '%s'", existing)
	}

	// Build the paths
	paths := backend.SpecialPaths()
	if paths == nil {
		paths = new(logical.Paths)
	}

	// Create a mount entry
	me := &mountEntry{
		tainted:    false,
		backend:    backend,
		view:       view,
		rootPaths:  pathsToRadix(paths.Root),
		loginPaths: pathsToRadix(paths.Unauthenticated),
	}
	r.root.Insert(prefix, me)
	return nil
}

// Unmount is used to remove a logical backend from a given prefix
func (r *Router) Unmount(prefix string) error {
	r.l.Lock()
	defer r.l.Unlock()
	r.root.Delete(prefix)
	return nil
}

// Remount is used to change the mount location of a logical backend
func (r *Router) Remount(src, dst string) error {
	r.l.Lock()
	defer r.l.Unlock()

	// Check for existing mount
	raw, ok := r.root.Get(src)
	if !ok {
		return fmt.Errorf("no mount at '%s'", src)
	}

	// Update the mount point
	r.root.Delete(src)
	r.root.Insert(dst, raw)
	return nil
}

// Taint is used to mark a path as tainted. This means only RollbackOperation
// RenewOperation requests are allowed to proceed
func (r *Router) Taint(path string) error {
	r.l.Lock()
	defer r.l.Unlock()
	_, raw, ok := r.root.LongestPrefix(path)
	if ok {
		raw.(*mountEntry).tainted = true
	}
	return nil
}

// Untaint is used to unmark a path as tainted.
func (r *Router) Untaint(path string) error {
	r.l.Lock()
	defer r.l.Unlock()
	_, raw, ok := r.root.LongestPrefix(path)
	if ok {
		raw.(*mountEntry).tainted = false
	}
	return nil
}

// MatchingMount returns the mount prefix that would be used for a path
func (r *Router) MatchingMount(path string) string {
	r.l.RLock()
	mount, _, ok := r.root.LongestPrefix(path)
	r.l.RUnlock()
	if !ok {
		return ""
	}
	return mount
}

// MatchingView returns the view used for a path
func (r *Router) MatchingView(path string) *BarrierView {
	r.l.RLock()
	_, raw, ok := r.root.LongestPrefix(path)
	r.l.RUnlock()
	if !ok {
		return nil
	}
	return raw.(*mountEntry).view
}

// Route is used to route a given request
func (r *Router) Route(req *logical.Request) (*logical.Response, error) {
	// If the path doesn't contain any slashes and doesn't end in a slash,
	// then append the slash. This lets "foo" mean "foo/" at the root level
	// which is almost always what we want.
	if !strings.Contains(req.Path, "/") {
		req.Path += "/"
	}

	// Find the mount point
	r.l.RLock()
	mount, raw, ok := r.root.LongestPrefix(req.Path)
	r.l.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no handler for route '%s'", req.Path)
	}
	defer metrics.MeasureSince([]string{"route", string(req.Operation),
		strings.Replace(mount, "/", "-", -1)}, time.Now())
	me := raw.(*mountEntry)

	// If the path is tainted, we reject any operation except for
	// Rollback and Revoke
	if me.tainted {
		switch req.Operation {
		case logical.RevokeOperation, logical.RollbackOperation:
		default:
			return nil, fmt.Errorf("no handler for route '%s'", req.Path)
		}
	}

	// Determine if this path is an unauthenticated path before we modify it
	loginPath := r.LoginPath(req.Path)

	// Adjust the path to exclude the routing prefix
	original := req.Path
	req.Path = strings.TrimPrefix(req.Path, mount)
	if req.Path == "/" {
		req.Path = ""
	}

	// Attach the storage view for the request
	req.Storage = me.view

	// Hash the request token unless this is the token backend
	clientToken := req.ClientToken
	if !strings.HasPrefix(original, "auth/token/") {
		req.ClientToken = me.SaltID(req.ClientToken)
	}

	// If the request is not a login path, then clear the connection
	originalConn := req.Connection
	if !loginPath {
		req.Connection = nil
	}

	// Reset the request before returning
	defer func() {
		req.Path = original
		req.Connection = originalConn
		req.Storage = nil
		req.ClientToken = clientToken
	}()

	// Invoke the backend
	return me.backend.HandleRequest(req)
}

// RootPath checks if the given path requires root privileges
func (r *Router) RootPath(path string) bool {
	r.l.RLock()
	mount, raw, ok := r.root.LongestPrefix(path)
	r.l.RUnlock()
	if !ok {
		return false
	}
	me := raw.(*mountEntry)

	// Trim to get remaining path
	remain := strings.TrimPrefix(path, mount)

	// Check the rootPaths of this backend
	match, raw, ok := me.rootPaths.LongestPrefix(remain)
	if !ok {
		return false
	}
	prefixMatch := raw.(bool)

	// Handle the prefix match case
	if prefixMatch {
		return strings.HasPrefix(remain, match)
	}

	// Handle the exact match case
	return match == remain
}

// LoginPath checks if the given path is used for logins
func (r *Router) LoginPath(path string) bool {
	r.l.RLock()
	mount, raw, ok := r.root.LongestPrefix(path)
	r.l.RUnlock()
	if !ok {
		return false
	}
	me := raw.(*mountEntry)

	// Trim to get remaining path
	remain := strings.TrimPrefix(path, mount)

	// Check the loginPaths of this backend
	match, raw, ok := me.loginPaths.LongestPrefix(remain)
	if !ok {
		return false
	}
	prefixMatch := raw.(bool)

	// Handle the prefix match case
	if prefixMatch {
		return strings.HasPrefix(remain, match)
	}

	// Handle the exact match case
	return match == remain
}

// pathsToRadix converts a the mapping of special paths to a mapping
// of special paths to radix trees.
func pathsToRadix(paths []string) *radix.Tree {
	tree := radix.New()
	for _, path := range paths {
		// Check if this is a prefix or exact match
		prefixMatch := len(path) >= 1 && path[len(path)-1] == '*'
		if prefixMatch {
			path = path[:len(path)-1]
		}

		tree.Insert(path, prefixMatch)
	}

	return tree
}
