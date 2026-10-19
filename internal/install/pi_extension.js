// rex-amq-adapter-extension: 1
// Installed by "rex-amq-adapter setup". Remove it with "rex-amq-adapter remove".
//
// Delivers AMQ mail to this Pi session inside Pi's process: nothing is typed
// into the terminal. The adapter binary decides identity and what to
// announce; this file decides only how Pi receives it: a turn of its own when
// idle, steered into a running turn, held while a dialog waits on the user.
import { execFile } from "node:child_process";
import * as fs from "node:fs";

const BIN = "__REX_AMQ_ADAPTER__";
const DEBOUNCE_MS = 300;
// fs.watch on macOS coalesces and can drop events; the rescan bounds the delay.
const RESCAN_MS = 30000;

// hook runs one adapter hook and returns its JSON reply ({} on any failure:
// the adapter logs its own errors, and Pi must keep working without it).
function hook(event, input) {
	return new Promise((resolve) => {
		const child = execFile(BIN, ["hook", event, "--agent", "pi"], { timeout: 5000 }, (_err, stdout) => {
			try {
				resolve(JSON.parse(stdout || "{}"));
			} catch {
				resolve({});
			}
		});
		child.stdin.on("error", () => {});
		child.stdin.end(JSON.stringify(input));
	});
}

export default function (pi) {
	if (!process.env.REX_BLOCK) return; // outside Rex the adapter does nothing

	let sessionId = "";
	let watcher;
	let rescan;
	let debounce;
	let busy = false; // a run is active: deliver into it
	let continued = false; // this run was already continued for mail
	let prompting = 0; // dialogs waiting on the user: deliver nothing meanwhile

	const input = (extra) => ({ session_id: sessionId, agent_pid: process.pid, ...extra });

	const deliver = async () => {
		// Asking the adapter would mark the mail announced; while a dialog
		// waits on the user, leave it unannounced and deliver once it closes.
		if (prompting > 0) return;
		// idle lets the adapter remind of mail announced but never drained.
		const r = await hook("mail", input({ idle: !busy }));
		if (!r.notice) return;
		const message = { customType: "amq", content: r.notice, display: true };
		pi.sendMessage(message, busy ? { deliverAs: "steer" } : { triggerTurn: true });
	};
	const schedule = () => {
		clearTimeout(debounce);
		debounce = setTimeout(deliver, DEBOUNCE_MS);
	};
	const disarm = () => {
		if (watcher) watcher.close();
		watcher = undefined;
		clearInterval(rescan);
		clearTimeout(debounce);
	};

	pi.on("session_start", async (_event, ctx) => {
		disarm(); // a reload or session switch starts over
		sessionId = ctx.sessionManager.getSessionId();
		const r = await hook("session-start", input({}));
		if (r.notice) pi.sendMessage({ customType: "amq", content: r.notice, display: false });
		if (!r.inbox) return;
		try {
			watcher = fs.watch(r.inbox, schedule);
		} catch {
			// the rescan still delivers
		}
		rescan = setInterval(deliver, RESCAN_MS);
		schedule(); // mail that arrived before the watch began
	});

	pi.on("agent_start", () => {
		busy = true;
		continued = false;
	});
	pi.on("agent_settled", () => {
		busy = false;
	});
	pi.on("ui_prompt_start", () => {
		prompting++;
	});
	pi.on("ui_prompt_end", () => {
		prompting = Math.max(0, prompting - 1);
		if (prompting === 0) schedule();
	});

	// The run is about to end with mail unread: continue it once.
	pi.on("agent_before_settle", async () => {
		const r = await hook("settle", input({ stop_hook_active: continued }));
		if (!r.notice) return;
		continued = true;
		return { entries: [{ type: "custom_message", customType: "amq", content: r.notice, display: true }], continue: true };
	});

	pi.on("session_shutdown", async (event) => {
		disarm();
		// Reload, new, resume, and fork keep the process and its pane.
		if (event.reason === "quit") await hook("session-end", input({}));
	});
}
