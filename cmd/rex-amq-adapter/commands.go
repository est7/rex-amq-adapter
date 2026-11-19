package main

import (
	"sync"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
)

// The registry is the one declaration of every public command: its words,
// argument and flag kinds, tier, guard, examples, and handler. Help, schema,
// validation, and dispatch all come from it.

var (
	harnesses  = []string{"claude", "codex", "agy", "pi"}
	mailKinds  = []string{"brainstorm", "review_request", "review_response", "question", "answer", "decision", "status", "todo"}
	priorities = []string{"urgent", "normal", "low"}
)

func f(name string, k cli.Kind, help string) cli.Flag {
	return cli.Flag{Name: name, Value: cli.Value{Kind: k}, Help: help}
}

func b(name, help string) cli.Flag { return f(name, cli.KindBool, help) }

func enum(name string, vals []string, help string) cli.Flag {
	return cli.Flag{Name: name, Value: cli.Value{Kind: cli.KindEnum, Enum: vals}, Help: help}
}

func num(name string, def string, min, max int, help string) cli.Flag {
	return cli.Flag{Name: name, Default: def, Value: cli.Value{Kind: cli.KindInt, Min: min, Max: max}, Help: help}
}

func req(fl cli.Flag) cli.Flag { fl.Required = true; return fl }

func dflt(fl cli.Flag, v string) cli.Flag { fl.Default = v; return fl }

func rep(fl cli.Flag) cli.Flag { fl.Repeat = true; return fl }

func a(name string, k cli.Kind, required bool, help string) cli.Arg {
	return cli.Arg{Name: name, Required: required, Value: cli.Value{Kind: k}, Help: help}
}

func rest(name string, k cli.Kind, required bool, help string) cli.Arg {
	r := a(name, k, required, help)
	r.Rest = true
	return r
}

func path(words ...string) []string { return words }

var (
	registryOnce sync.Once
	theRegistry  *cli.Registry
)

// registry is the CLI's command set, built and checked once.
func registry() *cli.Registry {
	registryOnce.Do(func() {
		r := &cli.Registry{Groups: groups(), Commands: commands(), HumanGate: app.HumanGate, Classify: app.Classify}
		if err := r.Finish(); err != nil {
			panic("registry: " + err.Error())
		}
		theRegistry = r
	})
	return theRegistry
}

func groups() []cli.Group {
	return []cli.Group{
		{Path: path("agent"), Summary: "agents in this Rex session: list, identity, pane title, roles, summon, retire"},
		{Path: path("mail"), Summary: "AMQ mail as you, through the adapter's gateway"},
		{Path: path("mail", "dlq"), Summary: "your dead-lettered mail"},
		{Path: path("epic"), Summary: "Epics: open, close, read, and hand back"},
		{Path: path("hub"), Summary: "the user's Hub directories, and ringing their personal agent"},
		{Path: path("crew"), Summary: "crews and the briefings their roles get"},
		{Path: path("trust"), Summary: "folder trust for every harness"},
		{Path: path("trust", "root"), Summary: "roots whose projects an Epic or summon trusts on first need"},
		{Path: path("stream"), Summary: "NDJSON streams of agents and mail"},
		{Path: path("install"), Summary: "the adapter's hooks, skill, and harness settings"},
		{Path: path("skill"), Summary: "the agent guide, by topic"},
	}
}

func commands() []*cli.Command {
	return []*cli.Command{
		// agent
		{
			Path: path("agent", "list"), Summary: "list the agents of this Rex session (or S): handle, kind, role, tab, state, unread",
			Flags: []cli.Flag{f("session", cli.KindSessionID, "list Rex session S instead of yours"), b("all", "also list departed agents reserved for a resume, and the narrator and personal agents ordinary views leave out")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, List: true, DefaultLimit: 50, MaxLimit: 100,
			DefaultFields: []string{"handle", "kind", "role", "tab", "state", "unread"},
			Result:        "a list of agents; --fields takes any core.Agent field (agent_session_id, cwd, block_id, lifecycle, delivery, ...)",
			Guardrails:    []string{"pure read: it never reconciles records or repairs wakers; `agent collect` does"},
			Examples:      []string{"rex-amq-adapter agent list", "rex-amq-adapter agent list --all --fields handle,block_id,delivery"},
			Run:           agentList,
		},
		{
			Path: path("agent", "env"), Summary: "the AMQ identity of the agent running it (AM_ROOT, AM_ME), and its Rex pane",
			Flags: []cli.Flag{b("sh", "print `export` lines to eval instead of the envelope; on failure it prints `false`, so `eval \"$(… --sh)\" && …` stops")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent,
			Result:     "{AM_ROOT, AM_ME, ME_UNATTENDED, role, REX_BLOCK, REX_SESSION}",
			Examples:   []string{"rex-amq-adapter agent env", `eval "$(rex-amq-adapter agent env --sh)"`},
			Guardrails: []string{"--sh is a shell protocol: text on stdout whatever the mode"},
			Run:        agentEnv,
		},
		{
			Path: path("agent", "title"), Summary: "show TASK after your role and handle in this pane's label; no TASK clears it",
			Args: []cli.Arg{rest("task", cli.KindSingleLine, false, "the task, a few words; put -- before one that starts with a dash")},
			Tier: cli.TierUndoable, Mutating: true, Idempotent: true, Audience: cli.AudienceAgent,
			Result:   "{label}",
			Examples: []string{`rex-amq-adapter agent title "review S1"`, "rex-amq-adapter agent title"},
			Run:      agentTitle,
		},
		{
			Path: path("agent", "join"), Summary: "take a role where you stand (and claim a crew for this Rex session), relabel your pane, and get the briefing",
			Args:  []cli.Arg{a("crew", cli.KindCrew, false, "claim this crew for the session; without --role, take its lead")},
			Flags: []cli.Flag{f("role", cli.KindRole, "the role to take")},
			Tier:  cli.TierUndoable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent, Doc: true,
			Result:   "{label, crew, briefing, markdown}; --text prints the briefing",
			Examples: []string{"rex-amq-adapter agent join --role reviewer", "rex-amq-adapter agent join pair"},
			Run:      agentJoin,
		},
		{
			Path: path("agent", "summon"), Summary: "send to a peer by handle or role: deliver to a live one, resume a departed one, or start one",
			Args: []cli.Arg{a("target", cli.KindTarget, true, "a handle (claude-02) or a role (reviewer)")},
			Flags: []cli.Flag{
				req(f("subject", cli.KindSingleLine, "the subject (a started peer's task label)")),
				req(f("body", cli.KindBody, "the message (a started peer's brief): text, @FILE, or -")),
				dflt(enum("kind", mailKinds, "the amq kind; only question, review_request, todo, and brainstorm start or resume an agent"), "question"),
				f("grant", cli.KindFile, "the user's grant file the brief cites (allows a start)"),
				f("asked", cli.KindBody, "the user's own words asking for a new agent (allows a start)"),
				enum("harness", harnesses, "the CLI of a start (default: by role)"),
				b("council", "a read-only role (reviewer, editor, qa, architect): one seat per harness of its routing, each asked for SCORE: <n>/10; when any seat cannot be mailed or launched, none is"),
				f("cwd", cli.KindDir, "the directory of a started agent, such as its Slice worktree"),
				b("split", "open a launched agent in a pane beside you, when the human asked"),
				dflt(f("timeout", cli.KindDuration, "how long to wait for a launched agent's adoption"), "45s"),
			},
			Tier: cli.TierLaunch, Mutating: true, DryRun: true, Input: true, Audience: cli.AudienceAgent,
			Result: "{action, handles, block, harness, passed_over, startup, ...}; refused exits 3 (cap_reached 5), not adopted exits 7",
			Guardrails: []string{"never from a personal agent; a request kind only; no cycle; depth 2 at most; 6 live agents per session",
				"a new agent needs --asked (the user's words) or --grant", "allowed under ME_UNATTENDED=1: Epics coordinate through it"},
			Examples: []string{`rex-amq-adapter agent summon reviewer --subject "review S1" --body @brief.md --grant grant.md --kind review_request`,
				`rex-amq-adapter agent summon claude-02 --subject "question" --body "Which base?" --dry-run`},
			Run: agentSummon,
		},
		{
			Path: path("agent", "await"), Summary: "wait for the agent started in a block, then report its startup",
			Args:  []cli.Arg{a("block", cli.KindBlockID, true, "the block a summon or epic open started")},
			Flags: []cli.Flag{dflt(f("timeout", cli.KindDuration, "how long to wait: 90s, 2m, or seconds"), "30s")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "the startup; anything but adopted exits 7 (not_adopted) with it in data",
			Examples: []string{"rex-amq-adapter agent await block:01a1eac4-f234-7985-8bef-2ea92c753265", "rex-amq-adapter agent await block:01a1eac4-f234-7985-8bef-2ea92c753265 --timeout 2m"},
			Run:      agentAwait,
		},
		{
			Path: path("agent", "retire"), Summary: "close a finished peer of this Rex session: depart it, retire its waker, close its pane",
			Args:  []cli.Arg{a("handle", cli.KindHandle, true, "the peer to retire")},
			Flags: []cli.Flag{b("force", "only when the user says so: retire it while it works or holds unread mail")},
			Tier:  cli.TierRecoverable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent,
			Result:     "{handle, block, action, pane, ...}; not at rest exits 5 (busy)",
			Guardrails: []string{"never a narrator, personal, or orchestrator agent, nor yourself", "--force skips only the busy check, and only the user authorizes it"},
			Examples:   []string{"rex-amq-adapter agent retire codex-03", "rex-amq-adapter agent retire codex-03 --dry-run"},
			Run:        agentRetire,
		},
		{
			Path: path("agent", "collect"), Summary: "reconcile records with Rex and bring back dead wakers (this session in Rex, every record outside)",
			Tier: cli.TierUndoable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent,
			Result:   "{scope, session_id}; a record that failed is a warning (meta.complete=false)",
			Examples: []string{"rex-amq-adapter agent collect", "rex-amq-adapter agent collect --dry-run"},
			Run:      agentCollect,
		},
		// mail
		{
			Path: path("mail", "send"), Summary: "send mail as you; the gateway decides, then each recipient's waker is checked",
			Flags: []cli.Flag{
				req(f("to", cli.KindHandles, "recipient handles, comma-separated")),
				req(f("subject", cli.KindSingleLine, "the subject")),
				req(f("body", cli.KindBody, "the message: text, @FILE, or -")),
				enum("kind", mailKinds, "the amq kind"),
				enum("priority", priorities, "the priority"),
				f("control", cli.KindEventID, "(a narrator) relay a user-control directive under this request id"),
				f("thread", cli.KindThreadID, "the thread"),
				f("refs", cli.KindMessageIDs, "related message ids"),
			},
			Tier: cli.TierOutward, Mutating: true, DryRun: true, Input: true, Audience: cli.AudienceAgent,
			Result:     "amq's send result: {id, thread, to, subject, ...}",
			Guardrails: []string{"Epics do not mail each other; a narrator takes no member mail", "allowed under ME_UNATTENDED=1"},
			Examples:   []string{`rex-amq-adapter mail send --to claude-02 --subject "done" --body "S1 is ready"`, "cat <<'EOF' | rex-amq-adapter mail send --input -\n{\"to\":\"claude-02\",\"subject\":\"done\",\"body\":\"S1 is ready\"}\nEOF"},
			Run:        mailSend,
		},
		{
			Path: path("mail", "reply"), Summary: "reply to a message in your mailbox",
			Flags: []cli.Flag{req(f("id", cli.KindMessageID, "the message to answer")), req(f("body", cli.KindBody, "the reply: text, @FILE, or -")),
				enum("kind", mailKinds, "the amq kind"), enum("priority", priorities, "the priority")},
			Tier: cli.TierOutward, Mutating: true, DryRun: true, Input: true, Audience: cli.AudienceAgent,
			Result:   "amq's reply result: {id, thread, to, ...}",
			Examples: []string{`rex-amq-adapter mail reply --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15 --body "yes"`, `rex-amq-adapter mail reply --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15 --body @answer.md --dry-run`},
			Run:      mailReply,
		},
		{
			Path: path("mail", "read"), Summary: "read one message by id (it stays where it is)",
			Flags: []cli.Flag{req(f("id", cli.KindMessageID, "the message")), num("max-body-bytes", "16384", 1, 262144, "cap the body; the full text spills to a 0600 file named in full_path")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{header, body, truncated, full_path, total_bytes}",
			Examples: []string{"rex-amq-adapter mail read --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15", "rex-amq-adapter mail read --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15 --max-body-bytes 4096"},
			Run:      mailRead,
		},
		{
			Path: path("mail", "drain"), Summary: "take your unread mail the gateway admits: each is moved to read and receipted",
			Flags: []cli.Flag{b("include-body", "include each body (capped by --max-body-bytes)"), num("limit", "20", 1, 100, "at most this many messages"),
				num("max-body-bytes", "16384", 1, 262144, "cap each body; the full text spills to a 0600 file")},
			Tier: cli.TierUndoable, Mutating: true, Audience: cli.AudienceAgent,
			Result:   "{items, total, truncated}: truncated says more unread mail may wait",
			Examples: []string{"rex-amq-adapter mail drain --include-body", "rex-amq-adapter mail drain --limit 5"},
			Run:      mailDrain,
		},
		{
			Path: path("mail", "list"), Summary: "list your mailbox: metadata and a cleaned snippet of at most 200 characters",
			Flags: []cli.Flag{b("new", "unread mail only"), b("cur", "read mail only"), f("from", cli.KindHandle, "only mail from this sender"), enum("kind", mailKinds, "only this kind")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, List: true,
			Result:   "a list of messages {id, from, subject, thread, created, box, snippet}",
			Examples: []string{"rex-amq-adapter mail list --new", "rex-amq-adapter mail list --from claude-02 --limit 5"},
			Run:      mailList,
		},
		{
			Path: path("mail", "thread"), Summary: "the messages of a thread in your mailbox you may read",
			Flags: []cli.Flag{req(f("id", cli.KindThreadID, "the thread, such as p2p/alice__bob")), b("include-body", "include each body (capped)"),
				num("limit", "20", 1, 100, "at most this many messages"), num("max-body-bytes", "16384", 1, 262144, "cap each body")},
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{items}; a thread with nothing you may read exits 4 (not_found, kind thread)",
			Examples: []string{"rex-amq-adapter mail thread --id p2p/claude-02__codex-01", "rex-amq-adapter mail thread --id p2p/claude-02__codex-01 --include-body"},
			Run:      mailThread,
		},
		{
			Path: path("mail", "trace"), Summary: "amq's delivery evidence for a message you may read",
			Args: []cli.Arg{a("id", cli.KindMessageID, true, "the message")},
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "amq's trace, scoped to what the gateway lets you see",
			Examples: []string{"rex-amq-adapter mail trace 2026-11-17T18-41-43.284Z_pid65763_92afbd15", "rex-amq-adapter mail trace 2026-11-17T18-41-43.284Z_pid65763_92afbd15 --json"},
			Run:      mailTrace,
		},
		{
			Path: path("mail", "dlq", "list"), Summary: "list your dead letters",
			Flags: []cli.Flag{b("new", "unread dead letters only"), b("cur", "inspected dead letters only")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, List: true,
			Result:   "a list of dead letters, as amq describes them",
			Examples: []string{"rex-amq-adapter mail dlq list", "rex-amq-adapter mail dlq list --new"},
			Run:      mailDLQList,
		},
		{
			Path: path("mail", "dlq", "read"), Summary: "read a dead letter and why it failed",
			Flags: []cli.Flag{req(f("id", cli.KindMessageID, "the dead letter"))},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "amq's dead letter",
			Examples: []string{"rex-amq-adapter mail dlq read --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15", "rex-amq-adapter mail dlq read --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15 --text"},
			Run:      mailDLQRead,
		},
		{
			Path: path("mail", "dlq", "retry"), Summary: "move dead letters back to your inbox (they meet the gateway again)",
			Flags: []cli.Flag{f("id", cli.KindMessageID, "the dead letter"), b("all", "every dead letter"), b("force", "retry beyond amq's max retries")},
			Tier:  cli.TierRecoverable, Mutating: true, DryRun: true, Audience: cli.AudienceAgent,
			Result:   "amq's retry result",
			Examples: []string{"rex-amq-adapter mail dlq retry --id 2026-11-17T18-41-43.284Z_pid65763_92afbd15", "rex-amq-adapter mail dlq retry --all --dry-run"},
			Run:      mailDLQRetry,
		},
		{
			Path: path("mail", "dlq", "purge"), Summary: "delete dead letters for good",
			Flags: []cli.Flag{f("older-than", cli.KindDuration, "only those older than this"), enum("confirm", []string{"purge"}, "the named confirmation: --confirm purge")},
			Tier:  cli.TierDestructive, Mutating: true, DryRun: true, Audience: cli.AudienceAgent,
			Result:     "amq's purge result",
			Guardrails: []string{"unrecoverable: needs --confirm purge (exit 2 confirmation_required without it); -y never confirms"},
			Examples:   []string{"rex-amq-adapter mail dlq purge --dry-run", "rex-amq-adapter mail dlq purge --older-than 168h --confirm purge"},
			Run:        mailDLQPurge,
		},
		// epic
		{
			Path: path("epic", "open"), Summary: "(a personal agent) open an Epic: a Rex session with its lead and narrator, linked to you",
			Args: []cli.Arg{a("name", cli.KindSingleLine, true, "the Epic's name (its Rex session label)")},
			Flags: []cli.Flag{
				req(f("cwd", cli.KindDir, "the repository the Epic's agents work in")),
				f("goal", cli.KindFileOrDash, "the goal contract the lead runs: a file, or - for stdin"),
				f("asked", cli.KindBody, "the user's own words asking for the Epic"),
				f("crew", cli.KindCrew, "the crew the Epic works as"),
				enum("harness", harnesses, "the lead's CLI"),
				dflt(f("timeout", cli.KindDuration, "how long to wait for both agents"), "90s"),
				f("op-id", cli.KindOpID, "names this request: a retry reconciles the Epic the first run started"),
			},
			Tier: cli.TierLaunch, Mutating: true, Idempotent: true, DryRun: true, Input: true, Audience: cli.AudienceAgent, Roles: []string{"personal"},
			Result:     "{session_id, op_id, replayed, lifecycle_state, lead {role, block, status, handle}, narrator, goal_path, grant_path, ...}; a partial start exits 7 with it in data",
			Guardrails: []string{"needs --goal, --asked, or both"},
			Examples:   []string{`rex-amq-adapter epic open s1 --cwd ~/projects/x --goal goal.md --asked "run the S1 goal"`, `rex-amq-adapter epic open s1 --cwd ~/projects/x --asked "fix the login bug" --op-id me:board:epic-7#open --dry-run`},
			Run:        epicOpen,
		},
		{
			Path: path("epic", "close"), Summary: "(a personal agent, on the user's words) close a linked Epic: kill its session, park its agents, unlink it",
			Args:  []cli.Arg{a("session", cli.KindSessionID, true, "the Epic's Rex session")},
			Flags: []cli.Flag{f("asked", cli.KindBody, "the user's own words asking to close it: the named confirmation"), b("force", "only when the user says so: close while its agents work"), f("op-id", cli.KindOpID, "names this request: a retry finishes the close")},
			Tier:  cli.TierRecoverable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent, Roles: []string{"personal"},
			Result:     "{session_id, parked, unlinked, op_id, lifecycle_state, replayed}",
			Guardrails: []string{"without --asked: exit 2 confirmation_required", "--force skips only the busy check, and only the user authorizes it"},
			Examples:   []string{`rex-amq-adapter epic close session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --asked "close it"`, `rex-amq-adapter epic close session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --asked "close it" --dry-run`},
			Run:        epicClose,
		},
		{
			Path: path("epic", "list"), Summary: "the Epics linked to a personal agent: lifecycle, hand-back, whether a view exists",
			Flags: []cli.Flag{f("hub", cli.KindDir, "outside a personal agent's pane: the Hub directory (default: the default Hub)"), f("personal", cli.KindHandle, "only Epics this personal agent opened or prefers (outside its pane: names its Hub)")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, Roles: []string{"personal", "human"}, Guard: cli.GuardHumanOutsideRex, List: true,
			Result:   "a list of Epics {session_id, name, dir, lifecycle, hand_back, has_view}",
			Examples: []string{"rex-amq-adapter epic list", "rex-amq-adapter epic list --personal claude-01 --fields session_id,lifecycle,hand_back"},
			Run:      epicList,
		},
		{
			Path: path("epic", "show"), Summary: "a session's view: members, obligations, recent mail, coverage gaps, hand-back, lifecycle",
			Args:  []cli.Arg{a("session", cli.KindSessionID, false, "the Epic session (a personal agent's linked Epic); default yours")},
			Flags: []cli.Flag{f("hub", cli.KindDir, "outside Rex: the Hub the Epic is linked to (default: the default Hub)"), f("personal", cli.KindHandle, "outside Rex: a live personal agent of that Hub")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, Roles: []string{"lead", "narrator", "personal", "human"}, Guard: cli.GuardHumanOutsideRex, Fields: true,
			Result:   "the view; narrow it with --fields lifecycle,obligations,hand_back",
			Examples: []string{"rex-amq-adapter epic show --fields lifecycle,obligations,hand_back", "rex-amq-adapter epic show session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"},
			Run:      epicShow,
		},
		{
			Path: path("epic", "refresh"), Summary: "(the narrator) rebuild the view without moving mail, keeping or replacing its narrative",
			Flags: []cli.Flag{f("summary", cli.KindBody, "the narrator's reading of the session: text or @FILE")},
			Tier:  cli.TierUndoable, Mutating: true, Idempotent: true, Audience: cli.AudienceAgent, Roles: []string{"narrator"},
			Result:   "the rebuilt view",
			Examples: []string{"rex-amq-adapter epic refresh", "rex-amq-adapter epic refresh --summary @summary.md"},
			Run:      epicRefresh,
		},
		{
			Path: path("epic", "messages"), Summary: "(the narrator) read messages of its session by id, receipt-free",
			Flags: []cli.Flag{req(f("ids", cli.KindMessageIDs, "message ids, comma-separated")), num("max-body-bytes", "16384", 1, 262144, "cap each body")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, Roles: []string{"narrator"},
			Result:   "{messages, coverage_gaps}",
			Examples: []string{"rex-amq-adapter epic messages --ids 2026-11-17T18-41-43.284Z_pid65763_92afbd15", "rex-amq-adapter epic messages --ids a1,b2 --max-body-bytes 4096"},
			Run:      epicMessages,
		},
		{
			Path: path("epic", "grant"), Summary: "the Epic's notarized grant: the user's words and the contract, its sha256 checked again",
			Args: []cli.Arg{a("session", cli.KindSessionID, false, "the Epic session; default yours")},
			Tier: cli.TierRead, Audience: cli.AudienceAgent, Roles: []string{"lead", "personal", "human"}, Guard: cli.GuardHumanOutsideRex,
			Result:   "{session_id, relayed_by, relayed_at, asked, contract, contract_sha256, contract_sha256_ok, contract_note, markdown}; contract_sha256_ok is false when the contract changed, cannot be read, or the grant names none",
			Examples: []string{"rex-amq-adapter epic grant", "rex-amq-adapter epic grant session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --text"},
			Doc:      true,
			Run:      epicGrant,
		},
		{
			Path: path("epic", "handback"), Summary: "(an Epic lead, when its run ends) record its whole final report as the session's hand-back",
			Args: []cli.Arg{a("report", cli.KindBody, true, "the report: text, @FILE, or -")},
			Flags: []cli.Flag{rep(f("artifact", cli.KindArtifact, "REL=@SRC: a byte-for-byte artifact (design Epics); at most 16, 256 KiB each, 1 MiB in all")),
				f("op-id", cli.KindOpID, "names this hand-back: a retry is changed:false, another hand-back under it is a conflict")},
			Tier: cli.TierUndoable, Mutating: true, Idempotent: true, DryRun: true, Input: true, Audience: cli.AudienceAgent, Roles: []string{"lead"},
			Result: "{by, at, bytes, seq, digest, changed, artifacts: [{name, sha256, bytes}], view}",
			Guardrails: []string{"a new hand-back replaces the last; the narrator is woken once it is stored",
				"the same report with the same artifact bytes again is changed:false; artifacts that changed are a new seq",
				"a symbolic link, directory, device, or FIFO as an artifact source is invalid_input"},
			Examples: []string{"rex-amq-adapter epic handback @hand-back.md", "rex-amq-adapter epic handback @report.md --artifact spec.md=@/tmp/design/spec.md --artifact 01-cache.md=@/tmp/design/01-cache.md"},
			Run:      epicHandback,
		},
		{
			Path: path("epic", "artifacts"), Summary: "the artifact manifests an Epic's hand-backs carry",
			Args:  []cli.Arg{a("session", cli.KindSessionID, true, "the Epic session")},
			Flags: []cli.Flag{num("seq", "0", 0, 1000000, "one revision (0: the latest)")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, Roles: []string{"personal", "human"}, Guard: cli.GuardHumanOutsideRex,
			Result:   "a list of manifests {session_id, seq, by, at, report_digest, artifacts: [{name, sha256, bytes}]}",
			Examples: []string{"rex-amq-adapter epic artifacts session:01a1eac4-f222-7d79-9291-9a8a86d39ec3", "rex-amq-adapter epic artifacts session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --seq 2"},
			Run:      epicArtifacts,
		},
		{
			Path: path("epic", "materialize"), Summary: "write an Epic's artifacts into a directory, byte for byte",
			Args:  []cli.Arg{a("session", cli.KindSessionID, true, "the Epic session")},
			Flags: []cli.Flag{req(f("into", cli.KindOutDir, "the directory to write")), num("seq", "0", 0, 1000000, "one revision (0: the latest)"), b("replace", "replace files that changed locally")},
			Tier:  cli.TierUndoable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent, Roles: []string{"personal", "human"}, Guard: cli.GuardHumanOutsideRex,
			Result:     "{session_id, seq, into, changed, atomic, complete, items: [{name, sha256, bytes, status}]}; writing into an existing directory is a non-atomic batch, meta.complete=false",
			Guardrails: []string{"a file changed since it was last materialized is a conflict (exit 5) and nothing is written, unless --replace", "a symbolic link in --into's path is invalid_input"},
			Examples:   []string{"rex-amq-adapter epic materialize session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --into ./design --dry-run", "rex-amq-adapter epic materialize session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --into ./design --seq 2"},
			Run:        epicMaterialize,
		},
		{
			Path: path("epic", "link"), Summary: "(a personal agent) let this session and an Epic's narrator exchange mail",
			Args: []cli.Arg{a("session", cli.KindSessionID, true, "the Epic session")},
			Tier: cli.TierUndoable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent, Roles: []string{"personal"},
			Result:   "{personal_session, epic_session, changed}",
			Examples: []string{"rex-amq-adapter epic link session:01a1eac4-f222-7d79-9291-9a8a86d39ec3", "rex-amq-adapter epic link session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --dry-run"},
			Run:      epicLink,
		},
		{
			Path: path("epic", "unlink"), Summary: "(a personal agent) revoke such a link",
			Args: []cli.Arg{a("session", cli.KindSessionID, true, "the Epic session")},
			Tier: cli.TierUndoable, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent, Roles: []string{"personal"},
			Result:   "{personal_session, epic_session, changed}",
			Examples: []string{"rex-amq-adapter epic unlink session:01a1eac4-f222-7d79-9291-9a8a86d39ec3", "rex-amq-adapter epic unlink session:01a1eac4-f222-7d79-9291-9a8a86d39ec3 --dry-run"},
			Run:      epicUnlink,
		},
		// hub
		{
			Path: path("hub", "list"), Summary: "the Hub directories: ME_HUB, else config.toml hub_dirs, else ~/me",
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{items: [{path, source}], configured, config}",
			Examples: []string{"rex-amq-adapter hub list", "rex-amq-adapter hub list --text"},
			Run:      hubList,
		},
		{
			Path: path("hub", "add"), Summary: "list DIR in config.toml hub_dirs",
			Args: []cli.Arg{a("dir", cli.KindDir, true, "the Hub directory")},
			Tier: cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{path, config, changed, hub_dirs}",
			Examples: []string{"rex-amq-adapter hub add ~/me", "rex-amq-adapter hub add ~/me --dry-run"},
			Run:      hubAdd,
		},
		{
			Path: path("hub", "remove"), Summary: "drop DIR from config.toml hub_dirs",
			Args: []cli.Arg{a("dir", cli.KindPath, true, "the Hub directory, which may be deleted already")},
			Tier: cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{path, config, changed, hub_dirs}",
			Examples: []string{"rex-amq-adapter hub remove ~/old-hub", "rex-amq-adapter hub remove ~/old-hub --dry-run"},
			Run:      hubRemove,
		},
		{
			Path: path("hub", "notify"), Summary: "(an outside producer) ring the live personal agent about an Inbox entry or Timer tick; never starts one",
			Flags: []cli.Flag{req(f("ref", cli.KindRef, "inbox:<id> or timer:<id>")), req(f("event-id", cli.KindEventID, "the event's id: a repeat is not sent again")),
				f("hub", cli.KindDir, "ring only a personal agent working in DIR"), f("personal", cli.KindHandle, "ring only this personal agent"),
				f("summary", cli.KindSingleLine, "a one-line hint, delivered quoted as untrusted data")},
			Tier: cli.TierOutward, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceAgent,
			Result:   "{status: notified|no_live_personal|deduped, to, session_id, message_id}; no_live_personal exits 0",
			Examples: []string{"rex-amq-adapter hub notify --ref inbox:42 --event-id inbox-42", `rex-amq-adapter hub notify --ref timer:7 --event-id timer-7-1 --summary "standup in 5" --dry-run`},
			Run:      hubNotify,
		},
		// crew
		{
			Path: path("crew", "list"), Summary: "the crews (claimed by a Rex session), their leads and members, and the roles of a session of no crew",
			Flags: []cli.Flag{f("cwd", cli.KindDir, "the directory whose crews apply (default: the current one)")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, List: true,
			Result:   "a list of crews {name, lead, members, roles, layer, conventions, purpose}, and roles",
			Examples: []string{"rex-amq-adapter crew list", "rex-amq-adapter crew list --cwd ~/projects/x --fields name,lead"},
			Run:      crewList,
		},
		{
			Path: path("crew", "show"), Summary: "the briefing an agent taking a role gets: conventions, role prompt, routing; changes nothing",
			Args:  []cli.Arg{a("crew", cli.KindCrew, false, "the crew; without --role, its lead")},
			Flags: []cli.Flag{f("role", cli.KindRole, "the role"), f("cwd", cli.KindDir, "the directory whose crews apply (default: the current one)")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, Doc: true,
			Result:   "{crew, role, lead, members, routing, markdown}; --text prints the markdown",
			Examples: []string{"rex-amq-adapter crew show pair --text", "rex-amq-adapter crew show --role reviewer"},
			Run:      crewShow,
		},
		// trust
		{
			Path: path("trust", "check"), Summary: "each harness's folder-trust verdict for DIR, and the agy settings derived for it",
			Args: []cli.Arg{a("dir", cli.KindDir, true, "the directory")},
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{dir, verdicts, settings, note}",
			Examples: []string{"rex-amq-adapter trust check .", `agy --settings "$(rex-amq-adapter trust check . --fields settings --json | jq -r .data.settings)"`},
			Fields:   true,
			Run:      trustCheck,
		},
		{
			Path: path("trust", "grant"), Summary: "the user's \"yes, I trust this folder\" for every harness at once",
			Args: []cli.Arg{rest("dir", cli.KindDir, true, "the directories")},
			Tier: cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{items: per-directory results}",
			Examples: []string{"rex-amq-adapter trust grant ~/projects/x", "rex-amq-adapter trust grant ~/projects/x --dry-run"},
			Run:      trustGrant,
		},
		{
			Path: path("trust", "root", "list"), Summary: "the declared trust roots",
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{items: [{path, ...}]}",
			Examples: []string{"rex-amq-adapter trust root list", "rex-amq-adapter trust root list --text"},
			Run:      trustRootList,
		},
		{
			Path: path("trust", "root", "add"), Summary: "declare roots whose projects an Epic or summon trusts on first need",
			Args: []cli.Arg{rest("dir", cli.KindDir, true, "the roots (never / or the home directory)")},
			Tier: cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{items: the roots afterwards}",
			Examples: []string{"rex-amq-adapter trust root add ~/projects", "rex-amq-adapter trust root add ~/projects --dry-run"},
			Run:      trustRootAdd,
		},
		{
			Path: path("trust", "root", "remove"), Summary: "drop declared roots",
			Args: []cli.Arg{rest("dir", cli.KindDir, true, "the roots")},
			Tier: cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{items: the roots afterwards}",
			Examples: []string{"rex-amq-adapter trust root remove ~/projects", "rex-amq-adapter trust root remove ~/projects --dry-run"},
			Run:      trustRootRemove,
		},
		// stream
		{
			Path: path("stream", "events"), Summary: "stream this Rex session's agents and mail as schema:1 NDJSON frames; read-only",
			Flags: []cli.Flag{f("session", cli.KindSessionID, "watch Rex session S"), b("tab", "watch your tab only"), b("all", "watch every agent of the server")},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent, Stream: true,
			Result:   "an envelope on a failure before it connects; schema:1 frames after (snapshot, upsert, remove, event, heartbeat); 130/143 on SIGINT/SIGTERM",
			Examples: []string{"rex-amq-adapter stream events", "rex-amq-adapter stream events --all"},
			Run:      streamEvents,
		},
		{
			Path: path("stream", "panel"), Summary: "the human's panel: stream events of a session plus its panel field",
			Flags: []cli.Flag{f("session", cli.KindSessionID, "watch Rex session S")},
			Tier:  cli.TierRead, Audience: cli.AudienceHuman, Guard: cli.GuardHuman, Stream: true,
			Result:   "as stream events, with a panel field on the snapshot and panel upserts",
			Examples: []string{"rex-amq-adapter stream panel", "rex-amq-adapter stream panel --session session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"},
			Run:      streamPanel,
		},
		// install
		{
			Path: path("install", "setup"), Summary: "install the skill, hooks, and harness settings for every found harness (or --agent)",
			Flags: []cli.Flag{rep(enum("agent", harnesses, "only this harness (repeatable)")), b("force", "replace what setup did not install")},
			Tier:  cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{binary, items, notes}",
			Examples: []string{"rex-amq-adapter install setup --dry-run", "rex-amq-adapter install setup --agent codex"},
			Run:      installSetup,
		},
		{
			Path: path("install", "status"), Summary: "what is installed, per harness, and Codex hooks awaiting approval",
			Flags: []cli.Flag{rep(enum("agent", harnesses, "only this harness (repeatable)"))},
			Tier:  cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{binary, items, codex_hooks}",
			Examples: []string{"rex-amq-adapter install status", "rex-amq-adapter install status --agent codex --text"},
			Run:      installStatus,
		},
		{
			Path: path("install", "remove"), Summary: "remove what setup installed",
			Flags: []cli.Flag{rep(enum("agent", harnesses, "only this harness (repeatable)")), b("force", "also remove what setup did not install")},
			Tier:  cli.TierUser, Mutating: true, Idempotent: true, DryRun: true, Audience: cli.AudienceHuman, Guard: cli.GuardHuman,
			Result:   "{binary, items, notes}",
			Examples: []string{"rex-amq-adapter install remove --dry-run", "rex-amq-adapter install remove --agent pi"},
			Run:      installRemove,
		},
		// skill
		{
			Path: path("skill", "list"), Summary: "the agent guide's topics",
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{items: [{topic, summary, bytes}]}",
			Examples: []string{"rex-amq-adapter skill list", "rex-amq-adapter skill list --text"},
			Run:      skillList,
		},
		{
			Path: path("skill", "show"), Summary: "one topic of the agent guide",
			Args: []cli.Arg{a("topic", cli.KindTopic, false, "the topic (default: the index of topics)")},
			Tier: cli.TierRead, Audience: cli.AudienceAgent, Doc: true,
			Result:   "{topic, markdown}; without a topic, the index; --text prints the markdown; an unknown topic exits 4 naming the topics",
			Examples: []string{"rex-amq-adapter skill show peers --text", "rex-amq-adapter skill show"},
			Run:      skillShow,
		},
		// top level
		{
			Path: path("schema"), Summary: "describe every command, one command, or the error codes; offline",
			Args: []cli.Arg{rest("command", cli.KindCommand, false, "a command's words, or errors")},
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "the schema: envelope contract, groups, commands, errors",
			Examples: []string{"rex-amq-adapter schema errors", "rex-amq-adapter schema agent summon"},
			Run:      schemaCmd,
		},
		{
			Path: path("version"), Summary: "the adapter's version",
			Tier: cli.TierRead, Audience: cli.AudienceAgent,
			Result:   "{version}",
			Examples: []string{"rex-amq-adapter version", "rex-amq-adapter version --text"},
			Run:      versionCmd,
		},
	}
}
