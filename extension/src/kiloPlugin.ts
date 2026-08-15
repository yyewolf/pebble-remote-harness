import * as vscode from 'vscode';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';

/**
 * Lifecycle for the Kilo plugin that feeds prh.
 *
 * Installing it is global: it loads into every kilo server on the machine,
 * for every project, including sessions with nothing to do with the harness.
 * That makes it a security-relevant act, so it is never silent — see
 * docs/plugin.md.
 *
 * Kilo loads plugins from the `plugin` array in `~/.config/kilo/opencode.json`,
 * and only from there — the array in `kilo.jsonc` is read and ignored, which
 * cost an afternoon to establish. Entries must be absolute paths; a bare
 * package name resolves against the npm registry, where this package is not
 * published. So installing means putting the plugin somewhere stable on disk
 * and naming that path, not shelling out to `kilo plugin -g`.
 */

/** The version the extension expects. Pinned, never floated. */
export const REQUIRED_VERSION = '0.1.0';

export const PACKAGE_NAME = '@yyewolf/prh-plugin';

export type PluginState =
  | { kind: 'installed'; version: string }
  | { kind: 'outdated'; version: string }
  | { kind: 'missing' }
  | { kind: 'linked'; version: string };

/**
 * Where the plugin lives once installed.
 *
 * Deliberately not inside the extension directory: that path carries the
 * extension version, so every extension update would strand the entry in
 * opencode.json pointing at a directory that no longer exists. A stable path
 * under Kilo's own config survives upgrades, and matches the layout Kilo
 * already uses for global modules.
 */
function installedPluginDir(): string {
  return path.join(kiloConfigDir(), 'node_modules', '@yyewolf', 'prh-plugin');
}

/**
 * Detects the plugin by reading the config file Kilo actually honours.
 *
 * An earlier version read `dependencies` out of `~/.config/kilo/package.json`,
 * which is what `kilo plugin -g` writes. That reports a working local install
 * as 'outdated' — the entry reads `"file:../../workspace/…"` and parses to a
 * junk version — so the extension nagged users whose setup was already fine.
 */
export async function detect(): Promise<PluginState> {
  const dir = installedPluginDir();

  const config = readOpencodeConfig();
  const plugins = Array.isArray(config.plugin) ? config.plugin : [];
  if (!plugins.includes(dir)) {
    return { kind: 'missing' };
  }

  let version: string;
  try {
    const manifest = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'));
    version = String(manifest.version ?? '');
  } catch {
    // Referenced but unreadable: the directory was deleted or a symlink
    // dangles. Treat as missing so the fix offered is a reinstall.
    return { kind: 'missing' };
  }

  // A symlink is a development checkout — the repo, edited in place. Report it
  // separately so install() never silently replaces someone's working tree
  // with a frozen copy.
  if (isSymlink(dir)) {
    return { kind: 'linked', version };
  }

  return version === REQUIRED_VERSION
    ? { kind: 'installed', version }
    : { kind: 'outdated', version };
}

/**
 * Installs the plugin after explicit confirmation.
 *
 * Copies the plugin shipped in the VSIX to a stable path, then adds that path
 * to the `plugin` array in opencode.json, preserving everything else in the
 * file.
 */
export async function install(extensionPath: string): Promise<void> {
  const dir = installedPluginDir();

  if (isSymlink(dir)) {
    // Someone linked their checkout here on purpose. Overwriting it would
    // delete a development setup and replace it with a stale copy.
    const proceed = await vscode.window.showWarningMessage(
      `${dir} is a symlink to a development checkout. Leave it alone and just ` +
      'make sure Kilo loads it?',
      { modal: true },
      'Register the symlink',
    );
    if (proceed !== 'Register the symlink') return;
    try {
      addPluginPath(dir);
      vscode.window.showInformationMessage(
        `${PACKAGE_NAME} registered from your checkout. Restart Kilo to load it.`,
      );
    } catch (e: any) {
      vscode.window.showErrorMessage(`Plugin install failed: ${e.message ?? e}`);
    }
    return;
  }

  const choice = await vscode.window.showWarningMessage(
    `Install ${PACKAGE_NAME}@${REQUIRED_VERSION} globally? ` +
    'This loads the plugin into every Kilo session on this machine, for every project.',
    { modal: true },
    'Install',
  );
  if (choice !== 'Install') return;

  try {
    const source = path.join(extensionPath, 'plugin');
    if (!fs.existsSync(path.join(source, 'package.json'))) {
      throw new Error(`no plugin bundled at ${source}`);
    }

    fs.rmSync(dir, { recursive: true, force: true });
    fs.mkdirSync(path.dirname(dir), { recursive: true });
    fs.cpSync(source, dir, { recursive: true });

    addPluginPath(dir);
    vscode.window.showInformationMessage(
      `${PACKAGE_NAME} installed. Restart Kilo for it to load.`,
    );
  } catch (e: any) {
    vscode.window.showErrorMessage(`Plugin install failed: ${e.message ?? e}`);
  }
}

/**
 * Removes the plugin.
 *
 * Also global, so it stops the harness in every window — say so before
 * doing it.
 */
export async function remove(): Promise<void> {
  const choice = await vscode.window.showWarningMessage(
    `Remove ${PACKAGE_NAME} globally? This stops the harness in every VSCode window.`,
    { modal: true },
    'Remove',
  );
  if (choice !== 'Remove') return;

  try {
    const dir = installedPluginDir();
    removePluginPath(dir);

    // Only delete what we created. A symlink is someone's checkout; unlinking
    // the reference is enough and deleting through it would be destructive.
    if (!isSymlink(dir)) {
      fs.rmSync(dir, { recursive: true, force: true });
    }
    vscode.window.showInformationMessage(
      `${PACKAGE_NAME} removed. Restart Kilo to unload it.`,
    );
  } catch (e: any) {
    vscode.window.showErrorMessage(`Plugin removal failed: ${e.message ?? e}`);
  }
}

/**
 * Nudges the user when the plugin is missing or stale.
 *
 * Non-modal and dismissible: a harness that nags on every window activation
 * is one people disable. It must never install as a side effect of noticing.
 */
export async function promptIfNeeded(state: PluginState, extensionPath: string): Promise<void> {
  // A linked checkout is a deliberate developer setup, not a problem to fix.
  if (state.kind === 'installed' || state.kind === 'linked') {
    return;
  }

  const dismissed = vscode.workspace.getConfiguration('prh').get<boolean>('pluginDismissed', false);
  if (dismissed) return;

  const msg = state.kind === 'outdated'
    ? `${PACKAGE_NAME} is at ${state.version}; ${REQUIRED_VERSION} is required.`
    : `${PACKAGE_NAME} is not installed. The harness needs it to receive prompts.`;

  const choice = await vscode.window.showInformationMessage(msg, 'Install', 'Dismiss');
  if (choice === 'Install') {
    await install(extensionPath);
  } else if (choice === 'Dismiss') {
    await vscode.workspace.getConfiguration('prh').update('pluginDismissed', true,
      vscode.ConfigurationTarget.Global);
  }
}

// -- opencode.json --------------------------------------------------------

interface OpencodeConfig {
  plugin?: unknown;
  [key: string]: unknown;
}

function kiloConfigDir(): string {
  const xdg = process.env.XDG_CONFIG_HOME;
  if (xdg) return path.join(xdg, 'kilo');
  return path.join(os.homedir(), '.config', 'kilo');
}

function opencodeConfigPath(): string {
  return path.join(kiloConfigDir(), 'opencode.json');
}

function readOpencodeConfig(): OpencodeConfig {
  try {
    return JSON.parse(fs.readFileSync(opencodeConfigPath(), 'utf8')) as OpencodeConfig;
  } catch {
    return {};
  }
}

/**
 * Rewrites opencode.json with our path present, leaving the rest untouched.
 *
 * This is the user's own Kilo configuration — models, providers, permissions —
 * so it is read, amended and written back rather than generated. Written via a
 * temp file and rename so a crash mid-write cannot truncate it.
 */
function addPluginPath(dir: string): void {
  const config = readOpencodeConfig();
  const plugins = Array.isArray(config.plugin) ? [...config.plugin] : [];
  if (!plugins.includes(dir)) {
    plugins.push(dir);
  }
  config.plugin = plugins;
  writeOpencodeConfig(config);
}

function removePluginPath(dir: string): void {
  const config = readOpencodeConfig();
  if (!Array.isArray(config.plugin)) return;
  config.plugin = config.plugin.filter((p) => p !== dir);
  writeOpencodeConfig(config);
}

function writeOpencodeConfig(config: OpencodeConfig): void {
  const target = opencodeConfigPath();
  fs.mkdirSync(path.dirname(target), { recursive: true });
  const tmp = `${target}.prh-tmp`;
  fs.writeFileSync(tmp, JSON.stringify(config, null, 2) + '\n', 'utf8');
  fs.renameSync(tmp, target);
}

function isSymlink(p: string): boolean {
  try {
    return fs.lstatSync(p).isSymbolicLink();
  } catch {
    return false;
  }
}
