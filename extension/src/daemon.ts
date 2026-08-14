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
   * Starts prh if it is not already running.
   *
   * TODO: implement.
   *  - resolve the binary: prh.binaryPath, else the bundled per-platform build
   *  - probe /v1/health first and adopt an already-running daemon instead of
   *    failing on a port clash
   *  - pass --listen from prh.bindAddress and prh.port
   *  - pipe stdout/stderr into this.output; prh logs slog text
   */
  async start(): Promise<void> {
    this.setState('starting');
    this.setState('failed');
    throw new Error('Daemon.start not implemented');
  }

  /**
   * Stops prh with SIGTERM, escalating to SIGKILL after a grace period.
   *
   * TODO: implement. Only kill what we spawned — an adopted daemon should
   * outlive the window that adopted it.
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
