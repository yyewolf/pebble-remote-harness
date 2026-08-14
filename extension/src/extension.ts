import * as vscode from 'vscode';
import { Daemon, DaemonState } from './daemon';
import * as kiloPlugin from './kiloPlugin';

let daemon: Daemon | undefined;

export async function activate(context: vscode.ExtensionContext): Promise<void> {
  const output = vscode.window.createOutputChannel('Pebble Remote Harness');
  context.subscriptions.push(output);

  daemon = new Daemon(output, context.secrets);
  context.subscriptions.push(daemon);

  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  status.command = 'prh.pair';
  context.subscriptions.push(status);
  context.subscriptions.push(daemon.stateChanged((s) => render(status, s)));
  render(status, daemon.current);
  status.show();

  context.subscriptions.push(
    vscode.commands.registerCommand('prh.start', () => daemon?.start()),
    vscode.commands.registerCommand('prh.stop', () => daemon?.stop()),
    vscode.commands.registerCommand('prh.pair', () => pair()),
    vscode.commands.registerCommand('prh.setPassword', () => setPassword()),
    vscode.commands.registerCommand('prh.devices', () => manageDevices()),
    vscode.commands.registerCommand('prh.showLog', () => output.show()),
    vscode.commands.registerCommand('prh.installPlugin', () => kiloPlugin.install()),
    vscode.commands.registerCommand('prh.removePlugin', () => kiloPlugin.remove()),
  );

  // Detect but never fix silently: installing the plugin affects every
  // project on the machine, so it stays a deliberate user action.
  void kiloPlugin.detect().then((state) => kiloPlugin.promptIfNeeded(state));

  if (vscode.workspace.getConfiguration('prh').get<boolean>('autoStart', true)) {
    try {
      await daemon.start();
    } catch (err) {
      output.appendLine(`autostart failed: ${err}`);
    }
  }
}

export function deactivate(): void {
  // Intentionally does not stop the daemon: prh outliving a window reload is
  // the point. Use "Pebble Harness: Stop daemon" to actually stop it.
}

function render(status: vscode.StatusBarItem, state: DaemonState): void {
  const faces: Record<DaemonState, [string, string]> = {
    stopped: ['$(circle-slash)', 'Harness stopped'],
    starting: ['$(sync~spin)', 'Harness starting'],
    running: ['$(watch)', 'Harness running'],
    unpaired: ['$(warning)', 'Harness running, no phone paired'],
    failed: ['$(error)', 'Harness failed — click for the log'],
  };
  const [icon, tooltip] = faces[state];
  status.text = `${icon} Pebble`;
  status.tooltip = tooltip;
}

/**
 * Shows what the phone needs to register: the reachable address and the
 * pairing password.
 *
 * TODO: implement.
 *  - pick the LAN address, not 127.0.0.1 — under VSCode Remote this must be
 *    the remote host's address, since that is where prh listens
 *  - render a QR encoding `prh://<host>:<port>?pw=<password>` so the
 *    companion can scan rather than have an IP typed into it
 *  - warn when bindAddress is loopback, which cannot work
 */
async function pair(): Promise<void> {
  await vscode.window.showInformationMessage('Pairing UI is not implemented yet.');
}

/**
 * Sets or regenerates the pairing password.
 *
 * TODO: implement. Default to a generated passphrase; changing it must
 * revoke every existing device token, since the old password is what those
 * devices were issued against.
 */
async function setPassword(): Promise<void> {
  await vscode.window.showInformationMessage('Password management is not implemented yet.');
}

/**
 * Lists paired devices and offers revocation.
 *
 * TODO: implement once prh exposes an admin endpoint for the device list.
 * That endpoint must be loopback-only — it is not part of the v1 surface the
 * companion talks to.
 */
async function manageDevices(): Promise<void> {
  await vscode.window.showInformationMessage('Device management is not implemented yet.');
}
