// The bridges probe: loaded for a test run only (--extensionDevelopmentPath
// on the remote), never installed. In the sandbox's extension host it waits
// for the other extensions (git adds its askpass settings to terminals),
// opens an integrated terminal, runs bridges.sh there, saves the output,
// and quits VS Code. Everything lives in /tmp/inv-e2e in the guest.
const vscode = require("vscode");
const fs = require("fs");

const dir = "/tmp/inv-e2e";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function activate() {
  try {
    // The extension host's own environment, for comparison with the terminal's.
    fs.writeFileSync(`${dir}/exthost-env.txt`, Object.keys(process.env).sort().join("\n") + "\n");
    await sleep(8000);
    const term = vscode.window.createTerminal({ name: "bridges" });
    term.show();
    // extra.sh, if the launcher copied one: a further check, run first.
    const extra = fs.existsSync(`${dir}/extra.sh`) ? `bash ${dir}/extra.sh > ${dir}/extra.txt 2>&1; ` : "";
    term.sendText(`${extra}env | cut -d= -f1 | sort > ${dir}/terminal-env.txt; bash ${dir}/bridges.sh > ${dir}/out.txt 2>&1; echo $? > ${dir}/done`);
    for (let i = 0; i < 120 && !fs.existsSync(`${dir}/done`); i++) await sleep(1000);
  } catch (e) {
    fs.writeFileSync(`${dir}/probe-error.txt`, String(e && e.stack || e));
  }
  await vscode.commands.executeCommand("workbench.action.quit");
}

module.exports = { activate, deactivate() {} };
