import * as vscode from 'vscode';
import * as os from 'os';

import { Daemon, DaemonState } from './daemon';
import * as kiloPlugin from './kiloPlugin';
import { encodeQr, renderQrSvg } from './qr';

let daemon: Daemon | undefined;

export async function activate(context: vscode.ExtensionContext): Promise<void> {
  const output = vscode.window.createOutputChannel('Pebble Remote Harness');
  context.subscriptions.push(output);

  daemon = new Daemon(output, context.secrets, context.extensionUri.fsPath);
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
    vscode.commands.registerCommand('prh.pair', () => pair(context)),
    vscode.commands.registerCommand('prh.setPassword', () => setPassword()),
    vscode.commands.registerCommand('prh.devices', () => manageDevices()),
    vscode.commands.registerCommand('prh.showLog', () => output.show()),
    vscode.commands.registerCommand('prh.installPlugin',
      () => kiloPlugin.install(context.extensionUri.fsPath)),
    vscode.commands.registerCommand('prh.removePlugin', () => kiloPlugin.remove()),
  );

  // Detect but never fix silently: installing the plugin affects every
  // project on the machine, so it stays a deliberate user action.
  void kiloPlugin.detect().then(
    (state) => kiloPlugin.promptIfNeeded(state, context.extensionUri.fsPath));

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
 * Opens a pairing window and shows the code the phone scans.
 *
 * The QR carries a freshly generated 32-byte pairing key, not the passphrase.
 * That distinction is the whole point: the phone signs its registration with
 * the key and prh returns the device secret encrypted under it, so anyone
 * watching the network sees a signature and a sealed blob and gains nothing.
 * Screen-to-camera is a channel an attacker on your LAN cannot reach; sending
 * a passphrase over plaintext HTTP threw that advantage away.
 *
 * The window is armed for a couple of minutes and closes the moment one device
 * enrols, so enrolment stops being a standing invitation to anyone who ever
 * learns the passphrase.
 */
async function pair(context: vscode.ExtensionContext): Promise<void> {
  if (!daemon) return;

  const cfg = vscode.workspace.getConfiguration('prh');
  const bindAddress = cfg.get<string>('bindAddress', '0.0.0.0');
  const port = daemon.currentPort;

  const lanAddr = await resolveLanAddress(bindAddress, context);
  if (lanAddr === undefined) {
    vscode.window.showWarningMessage(
      'prh has no non-loopback address for the phone to reach. Set prh.bindAddress to your LAN address.',
    );
    return;
  }
  if (lanAddr === null) {
    return; // the user dismissed the picker
  }

  // The TLS pin travels in the same out-of-band hop as the pairing key. A
  // phone that scanned the code learns both which server to trust and how to
  // prove itself to it; without the pin it has no way to tell prh from
  // anything else answering on that address, so pairing cannot proceed.
  const health = await daemon.health();
  const pin = health?.tls_pin;
  if (!pin) {
    vscode.window.showErrorMessage(
      'prh did not report a TLS fingerprint. Update the daemon binary and try again.',
    );
    return;
  }

  const ttlSec = 120;
  let pairing: { key: string; expiresAt: number };
  try {
    pairing = await daemon.openPairing(ttlSec);
  } catch (e) {
    vscode.window.showErrorMessage(`Could not start pairing: ${(e as Error).message}`);
    return;
  }

  const url = `prh://${lanAddr}:${port}?k=${pairing.key}&f=${pin}`;
  const qr = renderQrSvg(encodeQr(url).modules);

  const panel = vscode.window.createWebviewPanel(
    'prh.pair',
    'Pair a phone',
    vscode.ViewColumn.Active,
    { enableScripts: false },
  );

  panel.webview.html = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><style>
  body { font-family: sans-serif; padding: 24px; background: #fff; color: #000; }
  svg { display: block; margin: 0 auto; }
  code { font-size: 14px; word-break: break-all; }
  .warn { color: #a00; }
</style></head>
<body>
  <h2>Scan to pair your phone</h2>
  <p>Open the Pebble Remote Harness app and scan this code within
     <strong>${ttlSec} seconds</strong>. It stops working as soon as one phone
     pairs.</p>
  ${qr}
  <p>Or enter manually:</p>
  <p><code>${url}</code></p>
  <p class="warn">This code is a one-time secret. Do not paste it anywhere but
     the app.</p>
  <p>It carries prh's TLS fingerprint, so the phone can recognise this machine
     and refuse anything else.</p>
</body>
</html>`;

  // Closing the panel is the user saying they are done, whether or not a phone
  // enrolled. Shutting the window early costs nothing and narrows the gap.
  panel.onDidDispose(() => {
    void daemon?.closePairing();
  });

  // Deliberately not copied to the clipboard. The old flow copied a pairing
  // URL automatically, which put a live credential somewhere every app on the
  // machine can read, and left it there long after pairing finished.
  vscode.window.showInformationMessage(
    `Pairing open for ${ttlSec} seconds. Close the panel when you are done.`,
  );
}

/**
 * Sets or regenerates the pairing password.
 *
 * Default to a generated passphrase.
 *
 * Note that changing it does **not** revoke existing devices: they hold device
 * secrets issued at pairing and sign with those, never with the passphrase.
 * Rotating the passphrase only stops *new* pairings. Revoking a device is a
 * separate action against `devices.json`, and is still unimplemented here.
 */
async function setPassword(): Promise<void> {
  if (!daemon) return;

  const existing = await daemon.password();
  const placeholder = existing ? '(leave blank to keep current)' : '';
  const input = await vscode.window.showInputBox({
    prompt: 'Pairing password',
    password: true,
    placeHolder: placeholder,
    value: '',
  });

  let pw = input?.trim();
  if (pw === undefined) return; // cancelled
  if (pw === '' && existing) {
    pw = existing;
  } else if (pw === '') {
    pw = generatePassphrase();
  }

  try {
    await daemon.setPassword(pw);
    vscode.window.showInformationMessage(
      'Pairing password set. Existing devices keep working — they sign with their own secrets.',
    );
  } catch (e) {
    vscode.window.showErrorMessage(`Failed to set password: ${e}`);
  }
}

/**
 * Lists paired devices and offers revocation.
 */
async function manageDevices(): Promise<void> {
  if (!daemon) return;

  const h = await daemon.health();
  if (!h) {
    vscode.window.showWarningMessage('Daemon is not running.');
    return;
  }

  if (h.devices === 0) {
    vscode.window.showInformationMessage('No devices paired.');
    return;
  }

  // v1 has no admin device-list endpoint; surface the count and the stop
  // option. A future prh version should expose a loopback-only device list.
  const choice = await vscode.window.showInformationMessage(
    `${h.devices} device(s) paired, ${h.upstreams} upstream(s) active.`,
    'Stop daemon',
  );
  if (choice === 'Stop daemon') {
    await daemon.stop();
  }
}

// -- helpers --------------------------------------------------------------

/** Key under which the chosen LAN interface name is remembered. */
const PAIR_INTERFACE_KEY = 'prh.pairInterface';

interface LanCandidate {
  address: string;
  iface: string;
}

/**
 * Picks the address the phone will scan, prompting only when the host is
 * multi-homed and the answer is not already remembered.
 *
 * Returns:
 *   - the address as a string when one is chosen (or was remembered)
 *   - `null` when the user dismissed the picker
 *   - `undefined` when the host has no non-loopback address at all
 *
 * A specific `prh.bindAddress` is a decision already made in settings, so it
 * is used verbatim and never prompts. The wildcard (`0.0.0.0`/`::`) is where
 * ambiguity lives: the first non-loopback IPv4 in `os.networkInterfaces()`
 * iteration order is essentially arbitrary, and on a multi-homed box it is
 * usually a docker bridge or VPN that the phone cannot reach. So we rank real
 * interfaces first, prompt when several plausible ones remain, and remember
 * the *interface name* rather than the address — DHCP churns addresses, but
 * the interface the user picked stays put.
 */
async function resolveLanAddress(
  bindAddress: string,
  context: vscode.ExtensionContext,
): Promise<string | null | undefined> {
  if (bindAddress !== '0.0.0.0' && bindAddress !== '::') {
    return bindAddress;
  }

  const candidates = collectLanAddresses();
  if (candidates.length === 0) return undefined;
  if (candidates.length === 1) return candidates[0].address;

  // A remembered interface still on the list wins without a prompt.
  const remembered = context.globalState.get<string>(PAIR_INTERFACE_KEY);
  const rememberedCandidate = candidates.find((c) => c.iface === remembered);
  if (rememberedCandidate) return rememberedCandidate.address;

  const choice = await vscode.window.showQuickPick(
    candidates.map((c) => ({ label: c.address, description: c.iface })),
    {
      title: 'Pair over which network?',
      placeHolder: 'Pick the address your phone can reach',
    },
  );
  if (!choice) return null;

  const chosen = candidates.find((c) => c.address === choice.label);
  if (chosen) {
    await context.globalState.update(PAIR_INTERFACE_KEY, chosen.iface);
  }
  return choice.label;
}

/**
 * Every non-loopback, non-link-local IPv4 address on the host, ranked so the
 * interface the phone can actually reach sorts before the virtual plumbing.
 */
function collectLanAddresses(): LanCandidate[] {
  const seen = new Set<string>();
  const candidates: LanCandidate[] = [];
  const ifs = os.networkInterfaces();
  for (const name of Object.keys(ifs)) {
    for (const addr of ifs[name] ?? []) {
      if (addr.family !== 'IPv4' || addr.internal) continue;
      if (isLinkLocal(addr.address)) continue;
      if (seen.has(addr.address)) continue;
      seen.add(addr.address);
      candidates.push({ address: addr.address, iface: name });
    }
  }
  candidates.sort((a, b) => rankIface(a.iface) - rankIface(b.iface));
  return candidates;
}

/** 169.254.0.0/16 is link-local; a phone on the same LAN cannot route to it. */
function isLinkLocal(address: string): boolean {
  return address.startsWith('169.254.');
}

/**
 * Rough interface ranking. Virtual plumbing — docker bridges, veth pairs, VPN
 * tunnels, VM nets, macOS's internal awdl/llw/utun — is real IPv4 but not
 * what the phone should scan, so it sorts last. Ethernet and wifi (`en*`,
 * `eth*`, `wl*`, `wlan*`) sort first; everything else sits in between.
 */
function rankIface(name: string): number {
  const n = name.toLowerCase();
  if (/^(br|veth|virbr|tun|tap|utun|wg|docker|llw|awdl|vmnet|tailscale|zerotier)/.test(n)) {
    return 2;
  }
  if (/^(en|eth|wl|wlan|wwan)/.test(n)) {
    return 0;
  }
  return 1;
}

function generatePassphrase(): string {
  const words = ['alpha', 'bravo', 'charlie', 'delta', 'echo', 'foxtrot',
    'golf', 'hotel', 'india', 'juliet', 'kilo', 'lima', 'mike', 'november',
    'oscar', 'papa', 'quebec', 'romeo', 'sierra', 'tango', 'uniform',
    'victor', 'whiskey', 'xray', 'yankee', 'zulu'];
  const pick = () => words[Math.floor(Math.random() * words.length)];
  const num = () => Math.floor(Math.random() * 100);
  return `${pick()}-${pick()}-${num()}`;
}
