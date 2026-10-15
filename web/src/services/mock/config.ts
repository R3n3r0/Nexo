import { checkConfigValue, sensitiveMask } from '@/lib/config';

import type { ConfigService } from '../interfaces';
import type { ConfigDocument, ConfigSettings, ConfigSubsystemSchema } from '../types';
import type { MockIam } from './iam';
import { clone, problem, type MockContext } from './support';

/** The schema of the server (admin.configSchema). */
export function sampleConfigSchema(): ConfigSubsystemSchema[] {
  return [
    {
      name: 'api',
      description: 'S3 API behavior',
      keys: [
        {
          name: 'cors_allow_origin',
          type: 'list',
          default: '*',
          description: 'Origins allowed by CORS, comma separated',
          sensitive: false,
          applied: false,
        },
        {
          name: 'requests_max',
          type: 'int',
          default: '0',
          description: 'Maximum concurrent requests (0: automatic)',
          sensitive: false,
          applied: false,
        },
        {
          name: 'requests_deadline',
          type: 'duration',
          default: '10s',
          description: 'How long a request waits for a slot',
          sensitive: false,
          applied: false,
        },
        {
          name: 'root_access',
          type: 'bool',
          default: 'on',
          description:
            'Allow the root credentials on the S3 API (off: denied there; the console and the credentials derived from root keep working)',
          sensitive: false,
          applied: true,
        },
      ],
    },
    {
      name: 'browser',
      description: 'Web console',
      keys: [
        {
          name: 'session_duration',
          type: 'duration',
          default: '12h',
          description: 'Lifetime of a console session',
          sensitive: false,
          applied: false,
        },
        {
          name: 'upload_max_size',
          type: 'int',
          default: '67108864',
          description: 'Largest single-request upload in bytes',
          sensitive: false,
          applied: false,
        },
      ],
    },
    {
      name: 'audit_kafka',
      description: 'Audit log to Kafka (one JSON record per authenticated request)',
      keys: [
        {
          name: 'enable',
          type: 'bool',
          default: 'off',
          description: 'Enable the target',
          sensitive: false,
          applied: true,
        },
        {
          name: 'brokers',
          type: 'list',
          description: 'Brokers (host:port), comma separated',
          sensitive: false,
          applied: true,
        },
        {
          name: 'topic',
          type: 'string',
          description: 'Topic of the audit records',
          sensitive: false,
          applied: true,
        },
      ],
    },
    {
      name: 'audit_webhook',
      description: 'Audit log to a webhook (one JSON record per authenticated request)',
      keys: [
        {
          name: 'enable',
          type: 'bool',
          default: 'off',
          description: 'Enable the target',
          sensitive: false,
          applied: true,
        },
        {
          name: 'endpoint',
          type: 'url',
          description: 'URL receiving the audit records',
          sensitive: false,
          applied: true,
        },
        {
          name: 'auth_token',
          type: 'string',
          description: 'Bearer token sent to the endpoint',
          sensitive: true,
          applied: true,
        },
      ],
    },
    {
      name: 'logger',
      description: 'Server logs',
      keys: [
        {
          name: 'level',
          type: 'enum',
          default: 'info',
          values: ['debug', 'info', 'warn', 'error'],
          description: 'Minimum level',
          sensitive: false,
          applied: true,
        },
        {
          name: 'format',
          type: 'enum',
          default: 'text',
          values: ['text', 'json'],
          description: 'Output format',
          sensitive: false,
          applied: true,
        },
      ],
    },
    {
      name: 'notify_webhook',
      description: 'Bucket notifications to a webhook',
      keys: [
        {
          name: 'enable',
          type: 'bool',
          default: 'off',
          description: 'Enable the target',
          sensitive: false,
          applied: false,
        },
        {
          name: 'endpoint',
          type: 'url',
          description: 'URL receiving the events',
          sensitive: false,
          applied: false,
        },
        {
          name: 'auth_token',
          type: 'string',
          description: 'Bearer token sent to the endpoint',
          sensitive: true,
          applied: false,
        },
        {
          name: 'queue_dir',
          type: 'string',
          description:
            'Sub-directory of the persistent event queue, relative to the event queue root',
          sensitive: false,
          applied: false,
        },
        {
          name: 'queue_limit',
          type: 'int',
          default: '100000',
          description: 'Maximum queued events',
          sensitive: false,
          applied: false,
        },
      ],
    },
    {
      name: 'region',
      description: 'Server location',
      keys: [
        {
          name: 'name',
          type: 'string',
          default: 'us-east-1',
          description: 'Region name reported to clients',
          sensitive: false,
          applied: false,
        },
        {
          name: 'comment',
          type: 'string',
          description: 'Free text',
          sensitive: false,
          applied: false,
        },
      ],
    },
    {
      name: 'scanner',
      description: 'Background scanner (usage, lifecycle, healing checks)',
      keys: [
        {
          name: 'speed',
          type: 'enum',
          default: 'default',
          values: ['fastest', 'fast', 'default', 'slow', 'slowest'],
          description: 'Pace of the scan',
          sensitive: false,
          applied: false,
        },
        {
          name: 'cycle',
          type: 'duration',
          default: '1m',
          description: 'Pause between two scan cycles',
          sensitive: false,
          applied: false,
        },
      ],
    },
  ];
}

export interface MockConfigOptions {
  /** Stored settings by subsystem (unmasked). */
  config?: ConfigDocument;
}

/** In-memory configuration with the masking and validation rules of the server. */
export function createMockConfigService(
  ctx: MockContext,
  iam: MockIam,
  options: MockConfigOptions = {},
): ConfigService {
  const schema = sampleConfigSchema();
  const stored = new Map<string, ConfigSettings>(
    Object.entries(
      options.config ?? {
        region: { name: 'us-east-1' },
        notify_webhook: { enable: 'off', auth_token: 'demo-token' },
      },
    ).map(([name, settings]) => [name, { ...settings }]),
  );

  const guard = () => {
    if (!iam.allowed('admin:ConfigUpdate')) {
      throw problem(403, 'access_denied', 'Access denied: admin:ConfigUpdate is not allowed.');
    }
  };
  const subsystem = (name: string) => {
    // A notification or audit target may name its id (audit_webhook:<id>).
    const [prefix = name, id] = name.split(':', 2);
    const base = id !== undefined && id !== '' && /^(notify|audit)_/.test(prefix) ? prefix : name;
    const found = schema.find((s) => s.name === base);
    if (!found) {
      throw problem(400, 'invalid_argument', `Unknown configuration subsystem "${name}".`);
    }
    return found;
  };
  const masked = (name: string): ConfigSettings => {
    const s = subsystem(name);
    const out: ConfigSettings = {};
    for (const [key, value] of Object.entries(stored.get(name) ?? {})) {
      const sensitive = s.keys.find((k) => k.name === key)?.sensitive ?? false;
      out[key] = sensitive && value !== '' ? sensitiveMask : value;
    }
    return out;
  };
  /** Validates and drops the masked secrets, like admin.validate. */
  const validate = (name: string, settings: ConfigSettings): ConfigSettings => {
    const s = subsystem(name);
    const out: ConfigSettings = {};
    for (const [key, value] of Object.entries(settings)) {
      const k = s.keys.find((candidate) => candidate.name === key);
      if (!k) {
        throw problem(400, 'invalid_argument', `Unknown key "${key}" of subsystem ${name}.`);
      }
      if (k.sensitive && value === sensitiveMask) continue;
      if (checkConfigValue(k, value) !== undefined) {
        throw problem(400, 'invalid_argument', `${name}.${key}: "${value}" is not valid.`);
      }
      out[key] = value;
    }
    return out;
  };

  return {
    async list() {
      await ctx.delay();
      guard();
      return schema.map((s) => ({ name: s.name, settings: masked(s.name) }));
    },
    async schema() {
      await ctx.delay();
      guard();
      return clone(schema);
    },
    async get(name) {
      await ctx.delay();
      guard();
      return { name, settings: masked(name) };
    },
    async set(name, settings) {
      await ctx.delay();
      guard();
      const valid = validate(name, settings);
      const next = { ...(stored.get(name) ?? {}) };
      for (const [key, value] of Object.entries(valid)) {
        if (value === '') delete next[key];
        else next[key] = value;
      }
      stored.set(name, next);
      return { name, settings: masked(name) };
    },
    async reset(name, keys) {
      await ctx.delay();
      guard();
      subsystem(name);
      if (keys === undefined || keys.length === 0) {
        stored.delete(name);
        return;
      }
      const next = { ...(stored.get(name) ?? {}) };
      for (const key of keys) delete next[key];
      stored.set(name, next);
    },
    async exportAll() {
      await ctx.delay();
      guard();
      return Object.fromEntries([...stored.entries()].map(([name, s]) => [name, { ...s }]));
    },
    async importAll(document) {
      await ctx.delay();
      guard();
      const valid = Object.entries(document).map(
        ([name, settings]) => [name, validate(name, settings)] as const,
      );
      stored.clear();
      for (const [name, settings] of valid) {
        if (Object.keys(settings).length > 0) stored.set(name, { ...settings });
      }
    },
  };
}
