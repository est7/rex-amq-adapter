package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// An agent's state has two owners, so that what it is and where it runs
// never share a file:
//
//   - identities/<handle>.json: its Identity, who the agent is, from its first
//     adoption until collection forgets the handle.
//   - leases/<handle>.json: its Lease, the process and block it runs in now.
//     The file exists only while the agent runs; a departure archives it into
//     the Identity's LastPlacement and removes it.
//
// The writers below enforce four invariants and return an error, never a
// repaired state, when a write would break one:
//
//   - I1: an agent session (kind, session id) is in at most one Identity's
//     Sessions (PutIdentity scans every identity; there is no index to drift).
//   - I2: a handle holds at most one lease (the file is named for it).
//   - I3: a block (of one server) holds at most one live lease: acquiring
//     one on a block releases any other handle's lease there as
//     DepartReplaced.
//   - I4: a lease is released only by the epoch that holds it (ReleaseLease is
//     a compare-and-swap; a stale epoch is a no-op).
//
// The checks read and then write, so callers hold LockLifecycle, as every
// adoption, departure, and collection already does.

func (p Paths) identities() string { return filepath.Join(p.Home, "identities") }
func (p Paths) leases() string     { return filepath.Join(p.Home, "leases") }

// Identity is who an agent is: its handle, what started it and for what, and
// the agent sessions its conversation ran as. It says nothing about where the
// agent runs now; that is its Lease.
type Identity struct {
	Handle core.Handle `json:"handle"`
	Kind   core.Kind   `json:"kind"`
	// Role is what the agent is for ("reviewer", "orchestrator"; "" for none): the
	// launcher's pane label at adoption, or a crew role taken with join. It
	// chooses the agent's briefing and makes an orchestrator; the pane label
	// shows it before the handle.
	Role string `json:"role,omitempty"`
	// Task is what the agent said it is doing (core.Task), shown after the
	// handle in its pane label. It follows the handle, so a resume in another
	// pane shows it again and a newly named agent starts without one.
	Task string `json:"task,omitempty"`
	// Parent is the agent whose summon started this one ("" for an agent the
	// user started), Depth how many summons lie between the user and it (the
	// user's agents are 0), SummonedFor why the parent summoned it.
	Parent      core.Handle `json:"parent,omitempty"`
	Depth       int         `json:"depth,omitempty"`
	SummonedFor string      `json:"summoned_for,omitempty"`
	// Launched is set when the adapter launched the agent, and then Model and
	// Effort are what it launched it on (its routing, carried to adoption as
	// LaunchModelEnv and LaunchEffortEnv; empty for the CLI's default); a
	// resume reuses them. An agent the user started has none.
	Launched bool   `json:"launched,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Unattended is set when the agent runs unattended for an Epic (never on
	// a personal agent): it was launched with ME_UNATTENDED=1, or adopted in
	// a session whose EpicPolicy says so. A resume keeps it, and `env`
	// exports it to the agent's shells.
	Unattended bool `json:"unattended,omitempty"`
	// Cwd is the directory the agent runs in, from its foreground process at
	// adoption; a resume starts there.
	Cwd string `json:"cwd,omitempty"`
	// FirstAdoptedAt is when the handle was first adopted, kept across resumes
	// so tab-ownership order stays monotonic.
	FirstAdoptedAt time.Time `json:"first_adopted_at"`
	// Sessions are the agent sessions (the harness's conversation ids) this
	// handle ran as, in lineage order: the current one is last.
	Sessions []string `json:"sessions,omitempty"`
	Fence    Fence    `json:"fence"`
	// LastPlacement is where the agent last ran and why it left; nil until
	// its first lease ends.
	LastPlacement *Placement `json:"last_placement,omitempty"`
}

// Session is the agent session the handle runs as now, "" when none was
// reported.
func (id Identity) Session() string {
	if len(id.Sessions) == 0 {
		return ""
	}
	return id.Sessions[len(id.Sessions)-1]
}

// WithSession returns id running as agent session sid, which becomes the
// current one; an empty sid (a harness that reports none) changes nothing.
func (id Identity) WithSession(sid string) Identity {
	if sid == "" || id.Session() == sid {
		return id
	}
	id.Sessions = append(slices.DeleteFunc(slices.Clone(id.Sessions), func(s string) bool { return s == sid }), sid)
	return id
}

// Endpoint is the handle as the gateway sees it now: the session its fence
// places it in.
func (id Identity) Endpoint() core.Endpoint {
	return core.Endpoint{Handle: id.Handle, Role: id.Role, Server: id.Fence.Server, SessionID: id.Fence.SessionID, Generation: id.Fence.Generation}
}

// Fence is the Rex session a handle belongs to for the gateway
// (core.CanExchange), and how many times it moved between sessions: mail
// addressed to it in a session it left is fenced from it.
type Fence struct {
	Server    string `json:"server,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Generation counts the handle's moves between sessions.
	Generation int `json:"generation,omitempty"`
	// PlacedAt is when Generation last changed: the handle's copies of mail
	// sent before it are fenced from it.
	PlacedAt time.Time `json:"placed_at,omitempty"`
}

// moved returns f placed in session on server at at: a new generation when
// the session is another.
func (f Fence) moved(server, session string, at time.Time) Fence {
	if f.SessionID != "" && (f.SessionID != session || f.Server != server) {
		f.Generation++
		f.PlacedAt = at.UTC()
	}
	f.Server, f.SessionID = server, session
	return f
}

// Place is where in Rex a lease runs or ran: a block, and the window (tab)
// and session it was last seen in, on a server.
type Place struct {
	Server    string `json:"server"`
	SessionID string `json:"session_id,omitempty"`
	WindowID  string `json:"window_id,omitempty"`
	BlockID   string `json:"block_id"`
}

// Incarnation is one process: its pid, and when it started, so a pid the
// kernel reused for another process is not taken for it. StartTime is an
// opaque per-platform stamp compared only for equality; 0 when it could not
// be read (a remote server's process, or a platform without a reader).
type Incarnation struct {
	PID       int   `json:"pid"`
	StartTime int64 `json:"start_time,omitempty"`
}

// Same reports whether i and o are one process. An unknown start time
// cannot tell two processes with one pid apart, so it matches.
func (i Incarnation) Same(o Incarnation) bool {
	return i.PID == o.PID && (i.StartTime == 0 || o.StartTime == 0 || i.StartTime == o.StartTime)
}

// Lease is the agent of a handle running now: in which process and block.
type Lease struct {
	Handle core.Handle `json:"handle"`
	Place
	// ServerInstance is the Rex server's instance id when the lease was
	// acquired, so a server restart that reuses block ids is told apart.
	ServerInstance string      `json:"server_instance,omitempty"`
	Incarnation    Incarnation `json:"incarnation"`
	AcquiredAt     time.Time   `json:"acquired_at"`
	// Epoch counts the handle's acquisitions by a new process: it orders
	// releases (I4) and continues across departures.
	Epoch int64 `json:"epoch"`
	// Adopting is set while the adoption that acquired the lease has not
	// finished: the agent has a mailbox, but no waker or notice yet, and a
	// later hook finishes it.
	Adopting bool `json:"adopting,omitempty"`
}

// Placement is a lease that ended, kept in its Identity: where the agent
// ran, and why and when it left.
type Placement struct {
	Place
	Epoch      int64     `json:"epoch"`
	AcquiredAt time.Time `json:"acquired_at"`
	// DepartReason is DepartSessionEnd, DepartReplaced, DepartEpicClosed,
	// DepartRetired, or the binding check's code when collection found the
	// agent gone (block_gone, agent_gone, agent_left_pane).
	DepartReason string    `json:"depart_reason"`
	DepartedAt   time.Time `json:"departed_at"`
	// Unfinished is set while the departure's notices (core.LeftNotice's
	// wake of an orchestrator) are owed; collection finishes them.
	Unfinished bool `json:"unfinished,omitempty"`
}

// Reasons a departure records that are not a binding check's code.
const (
	DepartSessionEnd = "session_end" // the harness ended the session
	DepartReplaced   = "replaced"    // another agent or session took the block
	DepartEpicClosed = "epic_closed" // the personal agent closed its Epic's session
	DepartRetired    = "retired"     // a session-mate retired it as finished (rex-amq-adapter retire)
)

// Departure is how a lease ends.
type Departure struct {
	Reason string
	At     time.Time
	// Unfinished: the departure still owes notices (Placement.Unfinished).
	Unfinished bool
}

// ErrSessionTaken is returned when a write would put one agent session in
// two identities (I1).
var ErrSessionTaken = errors.New("agent session already belongs to another handle")

func (s Store) identityPath(h core.Handle) string {
	return filepath.Join(s.P.identities(), fileKey(string(h))+".json")
}

func (s Store) leasePath(h core.Handle) string {
	return filepath.Join(s.P.leases(), fileKey(string(h))+".json")
}

// IdentityOf returns h's identity; ok is false when the handle has none.
func (s Store) IdentityOf(h core.Handle) (Identity, bool, error) {
	var id Identity
	ok, err := readJSON(s.identityPath(h), &id)
	return id, ok, err
}

// Identities lists every identity, sorted by handle. An unreadable one is an
// error rather than skipped: it may hold the session another write must not
// take (I1).
func (s Store) Identities() ([]Identity, error) {
	var out []Identity
	err := readDir(s.P.identities(), func(name string, b []byte) error {
		var id Identity
		if err := json.Unmarshal(b, &id); err != nil {
			return fmt.Errorf("decode identity %s: %w", name, err)
		}
		out = append(out, id)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, err
}

// PutIdentity writes id as its handle's identity. It refuses an agent
// session another handle of the same kind holds (I1).
func (s Store) PutIdentity(id Identity) error {
	if id.Handle == "" {
		return errors.New("an identity needs a handle")
	}
	all, err := s.Identities()
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.Handle == id.Handle || o.Kind != id.Kind {
			continue
		}
		for _, sid := range id.Sessions {
			if slices.Contains(o.Sessions, sid) {
				return fmt.Errorf("%w: %s session %s is %s's, so %s cannot take it", ErrSessionTaken, id.Kind, sid, o.Handle, id.Handle)
			}
		}
	}
	return writeJSONIndent(s.identityPath(id.Handle), id)
}

// LeaseOf returns h's lease; ok is false when h's agent is not running.
func (s Store) LeaseOf(h core.Handle) (Lease, bool, error) {
	var l Lease
	ok, err := readJSON(s.leasePath(h), &l)
	return l, ok, err
}

// Leases lists every lease, sorted by handle.
func (s Store) Leases() ([]Lease, error) {
	var out []Lease
	err := readDir(s.P.leases(), func(name string, b []byte) error {
		var l Lease
		if err := json.Unmarshal(b, &l); err != nil {
			return fmt.Errorf("decode lease %s: %w", name, err)
		}
		out = append(out, l)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, err
}

// LeaseAt returns the live lease on block of server; ok is false when none
// is (I3 leaves at most one). An empty server matches any: a caller that
// knows only the block.
func (s Store) LeaseAt(server, block string) (Lease, bool, error) {
	leases, err := s.Leases()
	if err != nil {
		return Lease{}, false, err
	}
	for _, l := range leases {
		if l.BlockID == block && (server == "" || l.Server == server) {
			return l, true, nil
		}
	}
	return Lease{}, false, nil
}

// DepartedFrom returns the identity that last left block of server and is
// not running anywhere now; ok is false when none did. An empty server
// matches any.
func (s Store) DepartedFrom(server, block string) (Identity, bool, error) {
	ids, err := s.Identities()
	if err != nil {
		return Identity{}, false, err
	}
	leases, err := s.Leases()
	if err != nil {
		return Identity{}, false, err
	}
	live := make(map[core.Handle]bool, len(leases))
	for _, l := range leases {
		live[l.Handle] = true
	}
	var last Identity
	ok := false
	for _, id := range ids {
		p := id.LastPlacement
		if p == nil || p.BlockID != block || (server != "" && p.Server != server) || live[id.Handle] {
			continue
		}
		if !ok || p.DepartedAt.After(last.LastPlacement.DepartedAt) {
			last, ok = id, true
		}
	}
	return last, ok, nil
}

// AcquireLease records that l.Handle's agent runs as l.Incarnation in
// l.BlockID from at, and returns the lease written. The handle needs an
// identity. A lease the same process already holds keeps its epoch; any
// other, including a process of the same pid on another server, starts the
// next one, continuing past the handle's last placement.
// AcquiredAt is the caller's. The identity's fence
// moves to the lease's session (a new generation when it is another). Any
// other handle's lease on the block is released as DepartReplaced (I3).
func (s Store) AcquireLease(l Lease, at time.Time) (Lease, error) {
	if l.Handle == "" || l.BlockID == "" {
		return Lease{}, errors.New("a lease needs a handle and a block")
	}
	id, ok, err := s.IdentityOf(l.Handle)
	if err != nil {
		return Lease{}, err
	}
	if !ok {
		return Lease{}, fmt.Errorf("%s has no identity to lease", l.Handle)
	}
	leases, err := s.Leases()
	if err != nil {
		return Lease{}, err
	}
	l.Epoch = 1
	if id.LastPlacement != nil {
		l.Epoch = id.LastPlacement.Epoch + 1
	}
	for _, o := range leases {
		switch {
		case o.Handle == l.Handle && o.Server == l.Server && o.Incarnation.Same(l.Incarnation):
			l.Epoch = o.Epoch
		case o.Handle == l.Handle:
			l.Epoch = max(l.Epoch, o.Epoch+1)
		case o.BlockID == l.BlockID && o.Server == l.Server:
			if _, err := s.ReleaseLease(o.Handle, o.Epoch, Departure{Reason: DepartReplaced, At: at, Unfinished: true}); err != nil {
				return Lease{}, fmt.Errorf("release %s, replaced in %s: %w", o.Handle, l.BlockID, err)
			}
		}
	}
	id.Fence = id.Fence.moved(l.Server, l.SessionID, at)
	if err := s.PutIdentity(id); err != nil {
		return Lease{}, err
	}
	return l, writeJSONIndent(s.leasePath(l.Handle), l)
}

// FinishAdoption clears the Adopting mark of h's lease at epoch; a lease
// another epoch holds is left alone.
func (s Store) FinishAdoption(h core.Handle, epoch int64) error {
	l, ok, err := s.LeaseOf(h)
	if err != nil || !ok || l.Epoch != epoch || !l.Adopting {
		return err
	}
	l.Adopting = false
	return writeJSONIndent(s.leasePath(h), l)
}

// ReleaseLease ends h's lease if epoch still holds it (I4), archiving it
// into h's identity as its LastPlacement. released is false when there was no
// such lease. The archive is written before the lease is removed: a crash
// between leaves the agent live, and the next release finishes it.
func (s Store) ReleaseLease(h core.Handle, epoch int64, d Departure) (released bool, err error) {
	l, ok, err := s.LeaseOf(h)
	if err != nil || !ok || l.Epoch != epoch {
		return false, err
	}
	id, ok, err := s.IdentityOf(h)
	if err != nil {
		return false, err
	}
	if ok {
		id.LastPlacement = &Placement{Place: l.Place, Epoch: l.Epoch, AcquiredAt: l.AcquiredAt, DepartReason: d.Reason, DepartedAt: d.At.UTC(), Unfinished: d.Unfinished}
		if err := s.PutIdentity(id); err != nil {
			return false, err
		}
	}
	if err := os.Remove(s.leasePath(h)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// FinishDeparture records that h's last departure owes no more notices.
func (s Store) FinishDeparture(h core.Handle) error {
	id, ok, err := s.IdentityOf(h)
	if err != nil || !ok || id.LastPlacement == nil || !id.LastPlacement.Unfinished {
		return err
	}
	id.LastPlacement.Unfinished = false
	return s.PutIdentity(id)
}

// Moved records that the block h runs in, or last ran in when it is not
// running, is now in window of session (on the same server): its fence
// follows, a new generation when the session is another.
func (s Store) Moved(h core.Handle, session, window string, at time.Time) error {
	id, ok, err := s.IdentityOf(h)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s has no identity", h)
	}
	l, live, err := s.LeaseOf(h)
	if err != nil {
		return err
	}
	place := func(p *Place) {
		id.Fence = id.Fence.moved(p.Server, session, at)
		p.SessionID, p.WindowID = session, window
	}
	switch {
	case live:
		place(&l.Place)
	case id.LastPlacement != nil:
		place(&id.LastPlacement.Place)
	default:
		return nil
	}
	if err := s.PutIdentity(id); err != nil || !live {
		return err
	}
	return writeJSONIndent(s.leasePath(h), l)
}

// ForgetIdentity forgets handle h: its identity and its delivery history
// (the announcement ledger, the inject log, the title and task notes), so a
// later agent given h starts clean. A running handle is refused. Its mailbox
// is amq's and stays.
func (s Store) ForgetIdentity(h core.Handle) error {
	if _, live, err := s.LeaseOf(h); err != nil || live {
		if err == nil {
			err = fmt.Errorf("%s is running; only a departed handle is forgotten", h)
		}
		return err
	}
	for _, p := range []string{s.identityPath(h), s.ledgerPath(h), s.sentCursorPath(h), s.injectLog(h), s.titlePath(h), s.titleStatePath(h), s.taskAskedPath(h), s.turnErrorPath(h)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// AgentsChanged is when an identity or a lease was last written or removed:
// the newer modification time of their directories, which every write moves
// because it renames a file into place. Zero when neither exists yet.
func (s Store) AgentsChanged() (time.Time, error) {
	var newest time.Time
	for _, dir := range []string{s.P.identities(), s.P.leases()} {
		st, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	return newest, nil
}

// readDir calls f with each JSON file of dir; a missing dir has none. A file
// removed between the listing and its read (a lockless reader racing
// ReleaseLease or ForgetIdentity) is skipped: it was gone by the time it was
// read. Any other unreadable or undecodable file is an error.
func readDir(dir string, f func(name string, b []byte) error) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := f(e.Name(), b); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONIndent(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'))
}
