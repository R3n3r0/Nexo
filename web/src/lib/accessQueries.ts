/**
 * TanStack Query keys of the access rules, distributions, key groups and
 * certificates pages. The rules of a bucket live under its bucket keys
 * (['buckets', name, ...]) so that a deleted bucket drops them too.
 */
export const accessKeys = {
  /** Every block public access setting. */
  all: ['access'] as const,
  rules: (bucket: string) => ['buckets', bucket, 'access-rules'] as const,
  publicStatus: (bucket: string) => ['buckets', bucket, 'access-rules', 'public'] as const,
  blockPublic: (bucket?: string) => ['access', 'block-public', bucket ?? ''] as const,
  sampleKeys: (bucket: string) => ['objects', bucket, 'sample-keys'] as const,
};

export const distributionKeys = {
  all: ['distributions'] as const,
  list: ['distributions', 'list'] as const,
  one: (id: string) => ['distributions', 'one', id] as const,
  traffic: (id?: string) => ['distributions', 'traffic', id ?? ''] as const,
  resolve: (host: string) => ['distributions', 'resolve', host] as const,
  keyGroups: ['keygroups'] as const,
  certificates: ['certificates'] as const,
  dnsProviders: ['certificates', 'dns-providers'] as const,
};
