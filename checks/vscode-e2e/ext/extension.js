// The bridges probe: loaded for a test run only (--extensionDevelopmentPath
// on the remote), never installed. In the sandbox's extension host it waits
// for the other extensions (git adds its askpass settings to terminals),
// opens an integrated terminal, runs bridges.sh there, saves the output,
// and quits VS Code. Everything lives in /tmp/inv-e2e in the guest.
//
// Optional, by file in /tmp/inv-e2e (run.sh writes them):
//   extra.sh     run in the terminal first (output: extra.txt)
//   install.txt  extension ids to try installing, one per line (install-result.txt)
//   claude       Claude Code's extension: activate it and run its own binary
//                with the extension host's environment (claude-ext.txt)
//   hold         after done, write held and wait for release (up to 2 minutes),
//                so run.sh can look at the Mac while VS Code is still up
// Always: the extensions VS Code sees, and where each runs (extensions.txt).
const vscode = require("vscode");
const fs = require("fs");
const cp = require("child_process");

const dir = "/tmp/inv-e2e";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const has = (f) => fs.existsSync(`${dir}/${f}`);
const kinds = { 1: "ui", 2: "workspace" };

function listExtensions() {
  return vscode.extensions.all
    .filter((e) => !e.packageJSON.isBuiltin)
    .map((e) => `${e.id} ${e.packageJSON.version} kind=${kinds[e.extensionKind] || e.extensionKind} active=${e.isActive} at=${e.extensionUri.scheme}:${e.extensionUri.path}`)
    .sort()
    .join("\n") + "\n";
}

async function tryInstalls() {
  const out = [];
  for (const id of fs.readFileSync(`${dir}/install.txt`, "utf8").split("\n").map((s) => s.trim()).filter(Boolean)) {
    try {
      // A dialog nobody answers (e.g. trust a new publisher) would hang it.
      const r = await Promise.race([
        vscode.commands.executeCommand("workbench.extensions.installExtension", id).then(() => "installed"),
        sleep(45000).then(() => "no answer after 45s (a dialog?)"),
      ]);
      out.push(`${id}: ${r}`);
    } catch (e) {
      out.push(`${id}: refused: ${String((e && e.message) || e)}`);
    }
  }
  fs.writeFileSync(`${dir}/install-result.txt`, out.join("\n") + "\n");
}

async function checkClaude() {
  const out = [];
  // remote.SSH.defaultExtensions installs it after the window opens: wait.
  const t0 = Date.now();
  let ext;
  while (!(ext = vscode.extensions.getExtension("anthropic.claude-code")) && Date.now() - t0 < 240000) await sleep(2000);
  out.push(`waited ${Math.round((Date.now() - t0) / 1000)}s`);
  if (!ext) {
    out.push("anthropic.claude-code: not found");
  } else {
    out.push(`kind=${kinds[ext.extensionKind]} at=${ext.extensionUri.scheme}:${ext.extensionUri.path}`);
    try {
      await ext.activate();
      out.push(`activated: ${ext.isActive}`);
    } catch (e) {
      out.push(`activate failed: ${String((e && e.stack) || e)}`);
    }
    // The binary the extension runs, with this host's environment, as it would.
    const bin = `${ext.extensionPath}/resources/native-binary/claude`;
    for (const k of ["ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "NODE_EXTRA_CA_CERTS"]) out.push(`env ${k}=${process.env[k] || "(unset)"}`);
    const r = cp.spawnSync(bin, ["-p", "Reply with exactly: probe-ok"], { env: process.env, encoding: "utf8", timeout: 120000, cwd: "/home/agent" });
    out.push(`${bin} -p: status=${r.status} error=${r.error || ""}`);
    out.push(`stdout: ${(r.stdout || "").trim()}`);
    out.push(`stderr: ${(r.stderr || "").trim().slice(0, 2000)}`);
  }
  fs.writeFileSync(`${dir}/claude-ext.txt`, out.join("\n") + "\n");
}

async function activate() {
  try {
    // The extension host's own environment, for comparison with the terminal's.
    fs.writeFileSync(`${dir}/exthost-env.txt`, Object.keys(process.env).sort().join("\n") + "\n");
    await sleep(8000);
    if (has("install.txt")) await tryInstalls();
    if (has("claude")) await checkClaude();
    const term = vscode.window.createTerminal({ name: "bridges" });
    term.show();
    const extra = has("extra.sh") ? `bash ${dir}/extra.sh > ${dir}/extra.txt 2>&1; ` : "";
    term.sendText(`${extra}env | cut -d= -f1 | sort > ${dir}/terminal-env.txt; bash ${dir}/bridges.sh > ${dir}/out.txt 2>&1; echo $? > ${dir}/done`);
    for (let i = 0; i < 120 && !has("done"); i++) await sleep(1000);
    fs.writeFileSync(`${dir}/extensions.txt`, listExtensions());
    if (has("hold")) {
      fs.writeFileSync(`${dir}/held`, "");
      for (let i = 0; i < 120 && !has("release"); i++) await sleep(1000);
    }
  } catch (e) {
    fs.writeFileSync(`${dir}/probe-error.txt`, String((e && e.stack) || e));
  }
  await vscode.commands.executeCommand("workbench.action.quit");
}

module.exports = { activate, deactivate() {} };
