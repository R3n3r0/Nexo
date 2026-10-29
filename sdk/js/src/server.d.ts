export interface NexoConfig {
  /** URL of the Nexo server (the S3/Admin API listener): https://nexo.example.com:9000 */
  endpoint: string;
  /** An access key allowed nexo:IssuePlaybackToken (and admin:RevokePlaybackTokens to revoke). */
  accessKey: string;
  secretKey: string;
  sessionToken?: string;
  /** Region of the server (default us-east-1). */
  region?: string;
  fetch?: typeof fetch;
}

export interface IssueOptions {
  bucket: string;
  key: string;
  versionId?: string;
  /** The viewer (an account id of the application): the simultaneous streams are counted by subject. */
  subject: string;
  /** Text drawn over the video by the player (default: the subject). */
  watermark?: string;
  /** Validity in seconds (default 7200, at most the token TTL of the protection). */
  ttlSeconds?: number;
}

export interface PlaybackToken {
  token: string;
  sessionId: string;
  expiresAt: Date;
  /** Master playlist with the token, for any HLS player and for <nexo-player src>. */
  playlist: string;
  /** Page of the Nexo player, for an <iframe>. */
  player: string;
}

export interface NexoClient {
  issuePlaybackToken(opts: IssueOptions): Promise<PlaybackToken>;
  revokePlaybackSession(bucket: string, sessionId: string): Promise<void>;
  revokePlaybackSubject(bucket: string, subject: string): Promise<void>;
}

export function createNexoClient(config: NexoConfig): NexoClient;
export function signRequest(req: {
  method: string;
  url: URL;
  headers?: Record<string, string>;
  body?: string | Uint8Array;
  accessKey: string;
  secretKey: string;
  sessionToken?: string;
  region?: string;
  service?: string;
  date?: Date;
}): Record<string, string>;
