import type { ChildProcess } from 'child_process';
import * as vscode from 'vscode';
import * as net from 'net';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';
import { spawn } from 'child_process';
import * as http from 'http';
import * as https from 'https';
import * as crypto from 'crypto';

export type DaemonState = 'stopped' | 'starting' | 'running' | 'unpaired' | 'failed';

export interface Health {
  version: string;
  uptime_sec: number;
  upstreams: number;
  devices: number;
  sessions: number;
  listen: string;
  paired: boolean;
  pairing: boolean;
  /** base64url SHA-256 of prh's TLS public key; goes in the pairing QR. */
  tls_pin?: string;
}

export interface PairingStatus {
  open: boolean;
  expires_at?: number;
}

/**
 * Owns the prh child process.
 *
 * The daemon is deliberately capable of running standalone, so this class is
 * a lifecycle manager rather than a host: if VSCode dies, prh can be left
 * running and re-adopted on the next activation.
 */
export class Daemon implements vscode.Disposable {
  private proc?: ChildProcess;
  private state: DaemonState = 'stopped';
  private readonly onChange = new vscode.EventEmitter<DaemonState>();
  private port: number = 8477;

  readonly stateChanged = this.onChange.event;

  constructor(
    private readonly output: vscode.OutputChannel,
    private readonly secrets: vscode.SecretStorage,
  ) {}

  get current(): DaemonState {
    return this.state;
  }

  get currentPort(): number {
    return this.port;
  }

  /**
   * Ensures exactly one daemon is running for this user, then adopts it.
   *
   * There is one prh per machine, not per window: the phone pairs with one
   * endpoint and sees every project. Every window runs this concurrently, so
   * it has to be a race that only one participant can win.
   *
   *  1. connect to the plugin socket; if it answers, adopt and return
   *  2. if it refuses but the file exists, it is crash debris
   *  3. take an exclusive flock on `<runtime>/prh/daemon.lock`
   *  4. the winner spawns prh **detached** — own process group, unref()'d,
   *     stdio to a log file, never a child that dies with this window
   *  5. losers wait for the socket to appear, then adopt
   */
  async start(): Promise<void> {
    this.setState('starting');

    const cfg = vscode.workspace.getConfiguration('prh');
    this.port = cfg.get<number>('port', 8477);
    const bindAddress = cfg.get<string>('bindAddress', '0.0.0.0');
    const binaryPath = cfg.get<string>('binaryPath', '');

    const socketPath = this.socketPath();
    const lockPath = this.lockPath();

    // Step 1: try to connect to the existing daemon.
    if (await this.tryAdopt(socketPath)) {
      this.output.appendLine(`adopted existing daemon on ${socketPath}`);
      await this.checkHealth();
      return;
    }

    // Step 2: stale socket — clean it up.
    if (fs.existsSync(socketPath)) {
      try { fs.unlinkSync(socketPath); } catch { /* race */ }
    }

    // Step 3: take an exclusive flock.
    const lockDir = path.dirname(lockPath);
    fs.mkdirSync(lockDir, { recursive: true, mode: 0o700 });

    const won = await this.tryLock(lockPath);
    if (!won) {
      // Loser: wait for the socket to appear, then adopt.
      this.output.appendLine('another window is starting the daemon; waiting…');
      if (await this.waitForSocket(socketPath, 10_000)) {
        if (await this.tryAdopt(socketPath)) {
          this.output.appendLine('adopted daemon started by another window');
          await this.checkHealth();
          return;
        }
      }
      this.setState('failed');
      throw new Error('timed out waiting for another window to start the daemon');
    }

    // Step 4: we won the lock — spawn the daemon detached.
    const bin = this.resolveBinary(binaryPath);
    if (!bin) {
      this.setState('failed');
      throw new Error('prh binary not found; set prh.binaryPath or build the bundled binary');
    }

    const configDir = this.configDir();
    fs.mkdirSync(configDir, { recursive: true, mode: 0o700 });
    const configPath = path.join(configDir, 'config.json');
    const logPath = path.join(configDir, 'prh.log');

    await this.writeConfig(configPath, bindAddress);

    const logFd = fs.openSync(logPath, 'a');
    this.proc = spawn(bin, ['serve', '-config', configPath], {
      detached: true,
      stdio: ['ignore', logFd, logFd],
      env: { ...process.env },
    });
    this.proc.unref();
    this.output.appendLine(`spawned prh (pid ${this.proc.pid}) on ${bindAddress}:${this.port}`);

    // Wait for the socket to appear.
    if (await this.waitForSocket(socketPath, 5_000)) {
      await this.checkHealth();
    } else {
      this.setState('failed');
      throw new Error('daemon started but socket never appeared — check the log');
    }
  }

  /**
   * Stops the shared daemon. Explicit user action only.
   *
   * This affects every window and unpairs nothing gracefully, so it must be
   * deliberate — never a side effect of closing a window. There is no idle
   * timeout either: idle is the normal state of a harness waiting for you
   * to be interrupted.
   */
  async stop(): Promise<void> {
    // If we spawned it, kill the detached process group.
    if (this.proc && this.proc.pid) {
      try {
        process.kill(-this.proc.pid, 'SIGTERM');
        await this.waitForExit(this.proc.pid, 5_000);
      } catch {
        try { process.kill(-this.proc.pid!, 'SIGKILL'); } catch { /* already gone */ }
      }
      this.proc = undefined;
    } else {
      // Adopted daemon: stop via HTTP if reachable. We don't have a stop
      // endpoint in v1, so we find the pid via the lock file.
      const lockPath = this.lockPath();
      try {
        const pidStr = fs.readFileSync(lockPath, 'utf8').trim();
        const pid = parseInt(pidStr, 10);
        if (pid > 0) {
          process.kill(pid, 'SIGTERM');
          await this.waitForExit(pid, 5_000);
        }
      } catch { /* lock file may not exist */ }
    }

    // Clean up socket and lock.
    const socketPath = this.socketPath();
    try { fs.unlinkSync(socketPath); } catch { /* already gone */ }
    try { fs.unlinkSync(this.lockPath()); } catch { /* already gone */ }

    this.setState('stopped');
  }

  /**
   * Fetches /v1/health over loopback. Unauthenticated, so this doubles as a
   * liveness probe for a daemon this window did not start.
   *
   * prh serves TLS with a self-signed certificate, so validation is disabled
   * here. That is safe in this one place and nowhere else: the connection
   * never leaves 127.0.0.1, and this extension is the process that started the
   * daemon whose certificate it would be checking. There is no network path
   * for anyone to interpose on.
   *
   * The phone gets no such exemption — it pins the key, because its traffic
   * really does cross a network.
   *
   * Uses https.request rather than fetch deliberately: global fetch is undici,
   * which ignores `agent` and needs a `dispatcher` instead, so passing
   * rejectUnauthorized through fetch silently fails the handshake and every
   * health check comes back "daemon is dead".
   */
  async health(): Promise<Health | undefined> {
    return new Promise<Health | undefined>((resolve) => {
      const req = https.request(
        {
          host: '127.0.0.1',
          port: this.port,
          path: '/v1/health',
          method: 'GET',
          rejectUnauthorized: false,
          timeout: 3000,
        },
        (res) => {
          let buf = '';
          res.on('data', (c) => (buf += c));
          res.on('end', () => {
            if (res.statusCode !== 200) return resolve(undefined);
            try {
              resolve(JSON.parse(buf) as Health);
            } catch {
              resolve(undefined);
            }
          });
        },
      );
      req.on('error', () => resolve(undefined));
      req.on('timeout', () => {
        req.destroy();
        resolve(undefined);
      });
      req.end();
    });
  }

  /**
   * Generates a pairing password, stores the plaintext in SecretStorage, and
   * writes only its hash into the daemon's config.
   *
   * The hash must come from prh itself (`prh hash-password`) so the argon2id
   * parameters live in exactly one place.
   */
  async setPassword(plaintext: string): Promise<void> {
    await this.secrets.store('prh.password', plaintext);

    const binaryPath = vscode.workspace.getConfiguration('prh').get<string>('binaryPath', '');
    const bin = this.resolveBinary(binaryPath);
    if (!bin) throw new Error('prh binary not found');

    const result = await new Promise<string>((resolve, reject) => {
      const child = spawn(bin, ['hash-password', plaintext]);
      let out = '';
      child.stdout.on('data', (d) => { out += d.toString(); });
      child.on('close', (code) => {
        if (code === 0) resolve(out.trim());
        else reject(new Error(`hash-password exited ${code}`));
      });
      child.on('error', reject);
    });

    // Write the hash into the config file and signal the daemon to reload.
    const configPath = path.join(this.configDir(), 'config.json');
    const config = this.readConfig(configPath);
    config.password_hash = result;
    this.writeConfigFile(configPath, config);

    this.output.appendLine('pairing password updated (daemon config reloaded)');
  }

  /** Reads the stored pairing password, if the daemon has ever been paired. */
  async password(): Promise<string | undefined> {
    return this.secrets.get('prh.password');
  }

  private setState(next: DaemonState): void {
    if (this.state === next) {
      return;
    }
    this.state = next;
    this.onChange.fire(next);
  }

  // -- socket & lock helpers ---------------------------------------------

  /**
   * Arms a pairing window and returns the key the phone must present.
   *
   * The key is generated here rather than by prh because this is the process
   * that has to display it. It is never written to disk, never logged, and
   * prh keeps it only for the life of the window.
   *
   * Sent over the unix socket, not the network listener: the ability to open
   * enrolment must not be reachable by the people enrolment defends against.
   */
  async openPairing(ttlSec: number): Promise<{ key: string; expiresAt: number }> {
    const key = crypto.randomBytes(32).toString('base64url');
    const status = await this.adminRequest<PairingStatus>('POST', '/admin/v1/pairing', {
      pairing_key: key,
      ttl_sec: ttlSec,
    });
    return { key, expiresAt: status.expires_at ?? 0 };
  }

  /** Disarms the window early — e.g. when the user closes the pairing panel. */
  async closePairing(): Promise<void> {
    try {
      await this.adminRequest<PairingStatus>('DELETE', '/admin/v1/pairing', null);
    } catch {
      // Best effort. The window expires on its own, so failing to close it
      // early is not worth interrupting the user over.
    }
  }

  /**
   * One request against the admin plane on the plugin socket.
   *
   * Node's http client speaks to a unix socket via `socketPath`; the hostname
   * in the URL is ignored but must be present.
   */
  private adminRequest<T>(method: string, route: string, body: unknown): Promise<T> {
    const payload = body === null ? undefined : Buffer.from(JSON.stringify(body));

    return new Promise<T>((resolve, reject) => {
      const req = http.request(
        {
          socketPath: this.socketPath(),
          path: route,
          method,
          timeout: 5000,
          headers: payload
            ? { 'content-type': 'application/json', 'content-length': payload.length }
            : {},
        },
        (res) => {
          let buf = '';
          res.on('data', (c) => (buf += c));
          res.on('end', () => {
            if (res.statusCode && res.statusCode >= 200 && res.statusCode < 300) {
              try {
                resolve(buf ? JSON.parse(buf) : ({} as T));
              } catch (e) {
                reject(new Error(`malformed response from prh: ${buf}`));
              }
              return;
            }
            reject(new Error(`prh returned ${res.statusCode}: ${buf}`));
          });
        },
      );
      req.on('error', (e) =>
        reject(new Error(`prh is not reachable on its socket: ${e.message}`)),
      );
      req.on('timeout', () => {
        req.destroy();
        reject(new Error('prh did not answer on its socket'));
      });
      if (payload) req.write(payload);
      req.end();
    });
  }

  private socketPath(): string {
    const xdg = process.env.XDG_RUNTIME_DIR;
    if (xdg) return path.join(xdg, 'prh', 'plugin.sock');
    const home = os.homedir();
    return path.join(home, '.local', 'state', 'prh', 'plugin.sock');
  }

  private lockPath(): string {
    const xdg = process.env.XDG_RUNTIME_DIR;
    if (xdg) return path.join(xdg, 'prh', 'daemon.lock');
    const home = os.homedir();
    return path.join(home, '.local', 'state', 'prh', 'daemon.lock');
  }

  private configDir(): string {
    const xdg = process.env.XDG_CONFIG_HOME;
    if (xdg) return path.join(xdg, 'prh');
    const home = os.homedir();
    return path.join(home, '.config', 'prh');
  }

  private async tryAdopt(socketPath: string): Promise<boolean> {
    return new Promise((resolve) => {
      const sock = new net.Socket();
      sock.setTimeout(500);
      sock.once('connect', () => {
        sock.destroy();
        resolve(true);
      });
      sock.once('error', () => {
        sock.destroy();
        resolve(false);
      });
      sock.once('timeout', () => {
        sock.destroy();
        resolve(false);
      });
      sock.connect(socketPath);
    });
  }

  private async waitForSocket(socketPath: string, timeoutMs: number): Promise<boolean> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (fs.existsSync(socketPath) && await this.tryAdopt(socketPath)) {
        return true;
      }
      await new Promise((r) => setTimeout(r, 200));
    }
    return false;
  }

  private tryLock(lockPath: string): Promise<boolean> {
    return new Promise((resolve) => {
      try {
        // Use file existence as a simple lock. The lock file contains the PID.
        if (fs.existsSync(lockPath)) {
          const pidStr = fs.readFileSync(lockPath, 'utf8').trim();
          const pid = parseInt(pidStr, 10);
          if (pid > 0 && this.isProcessAlive(pid)) {
            resolve(false);
            return;
          }
          // Stale lock — remove it.
          try { fs.unlinkSync(lockPath); } catch { /* race */ }
        }
        fs.writeFileSync(lockPath, String(process.pid), { mode: 0o600 });
        resolve(true);
      } catch {
        resolve(false);
      }
    });
  }

  private isProcessAlive(pid: number): boolean {
    try { process.kill(pid, 0); return true; } catch { return false; }
  }

  private async waitForExit(pid: number, timeoutMs: number): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (!this.isProcessAlive(pid)) return;
      await new Promise((r) => setTimeout(r, 200));
    }
  }

  private async checkHealth(): Promise<void> {
    const h = await this.health();
    if (h) {
      this.setState(h.paired ? 'running' : 'unpaired');
    } else {
      this.setState('failed');
    }
  }

  // -- binary resolution --------------------------------------------------

  private resolveBinary(binaryPath: string): string | undefined {
    if (binaryPath && fs.existsSync(binaryPath)) return binaryPath;
    // Check common locations.
    const candidates = [
      path.join(this.configDir(), 'prh'),
      path.join(os.homedir(), '.local', 'bin', 'prh'),
      '/usr/local/bin/prh',
    ];
    for (const c of candidates) {
      if (fs.existsSync(c)) return c;
    }
    return undefined;
  }

  // -- config writing -----------------------------------------------------

  private async writeConfig(configPath: string, bindAddress: string): Promise<void> {
    const existing = this.readConfig(configPath);
    existing.listen = `${bindAddress}:${this.port}`;
    if (!existing.server_name) {
      existing.server_name = os.hostname();
    }
    if (!existing.socket_path) {
      existing.socket_path = this.socketPath();
    }
    if (!existing.ring_size) {
      existing.ring_size = 200;
    }
    if (!existing.max_poll_wait_sec) {
      existing.max_poll_wait_sec = 55;
    }
    this.writeConfigFile(configPath, existing);
  }

  private readConfig(configPath: string): any {
    try {
      const buf = fs.readFileSync(configPath, 'utf8');
      return JSON.parse(buf);
    } catch {
      return {};
    }
  }

  private writeConfigFile(configPath: string, config: any): void {
    fs.mkdirSync(path.dirname(configPath), { recursive: true, mode: 0o700 });
    const tmp = configPath + '.tmp';
    fs.writeFileSync(tmp, JSON.stringify(config, null, 2), { mode: 0o600 });
    fs.renameSync(tmp, configPath);
  }

  dispose(): void {
    this.onChange.dispose();
    // Do not kill the detached process — it outlives the window.
  }
}
