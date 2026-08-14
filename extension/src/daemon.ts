import type { ChildProcess } from 'child_process';
import * as vscode from 'vscode';

export type DaemonState = 'stopped' | 'starting' | 'running' | 'unpaired' | 'failed';

export interface Health {
  version: string;
  uptime_sec: number;
  upstreams: number;
  devices: number;
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

  readonly stateChanged = this.onChange.event;

  constructor(
    private readonly output: vscode.OutputChannel,
    private readonly secrets: vscode.SecretStorage,
  ) {}

  get current(): DaemonState {
    return this.state;
  }

  /**
   * Ensures exactly one daemon is running for this user, then adopts it.
   *
   * There is one prh per machine, not per window: the phone pairs with one
   * endpoint and sees every project. Every window runs this concurrently, so
   * it has to be a race that only one participant can win.
   *
   * TODO: implement.
   *  1. connect to the plugin socket; if it answers, adopt and return
   *  2. if it refuses but the file exists, it is crash debris
   *  3. take an exclusive flock on `<runtime>/prh/daemon.lock`
   *  4. the winner spawns prh **detached** — own process group, unref()'d,
   *     stdio to a log file, never a child that dies with this window
   *  5. losers wait for the socket to appear, then adopt
   *
   * Resolve the binary from prh.binaryPath, else the bundled per-platform
   * build. If an adopted daemon reports a different `listen` than this
   * window's settings, warn — do not restart a daemon other windows are on.
   */
  async start(): Promise<void> {
    this.setState('starting');
    this.output.appendLine('start: not implemented');
    this.setState('failed');
    throw new Error('Daemon.start not implemented');
  }

  /**
   * Stops the shared daemon. Explicit user action only.
   *
   * This affects every window and unpairs nothing gracefully, so it must be
   * deliberate — never a side effect of closing a window. There is no idle
   * timeout either: idle is the normal state of a harness waiting for you to
   * be interrupted.
   *
   * TODO: implement with SIGTERM, escalating to SIGKILL after a grace period.
   */
  async stop(): Promise<void> {
    throw new Error('Daemon.stop not implemented');
  }

  /**
   * Fetches /v1/health. Unauthenticated, so this doubles as a liveness probe
   * for a daemon this window did not start.
   *
   * TODO: implement against `http://127.0.0.1:${port}/v1/health`.
   */
  async health(): Promise<Health | undefined> {
    return undefined;
  }

  /**
   * Generates a pairing password, stores the plaintext in SecretStorage, and
   * writes only its hash into the daemon's config.
   *
   * TODO: implement. The hash must come from prh itself (`prh hash-password`)
   * so the argon2id parameters live in exactly one place.
   */
  async setPassword(_plaintext: string): Promise<void> {
    throw new Error('Daemon.setPassword not implemented');
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

  dispose(): void {
    this.onChange.dispose();
    this.proc?.kill();
  }
}
