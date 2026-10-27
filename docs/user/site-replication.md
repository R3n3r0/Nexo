# Site replication

Site replication keeps several Nexo deployments (sites) in sync, for disaster recovery or to serve users
from the closest region. Every site has the same:

- users, groups and access keys, with their secrets and their policies;
- named policies;
- buckets, with their versioning, object lock, policy, lifecycle, encryption, quota, tags, CORS and media
  rules;
- objects: every bucket replicates to the same bucket of every other site, active-active (see
  [bucket replication](replication.md)).

A change made on any site (a user created, a policy edited, a bucket deleted, an object written) reaches the
others within seconds. A site that was unreachable catches up when it comes back.

The configuration (`admin config`), the KMS keys, the notification targets, the distributions and the
certificates stay per site.

## Requirements

- Every site runs Nexo and is reachable from the others at its S3 API and its Admin API (the console
  listener).
- The sites **share the root credentials** (`NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD`): they call each other with
  them, and seal what they exchange with a key derived from them.
- The clocks are in sync (NTP): the most recent change wins.

## Create a group

Create an alias for each site, then group them from any of them:

```bash
nexoctl alias set site-a https://s3.a.example.com nexoadmin '…' --admin-url https://console.a.example.com
nexoctl alias set site-b https://s3.b.example.com nexoadmin '…' --admin-url https://console.b.example.com
nexoctl admin replicate add site-a site-b
```

The alias names become the names of the sites. The first site checks that it reaches every other one, saves the
group on all of them and starts a first round: it **only adds** what each site lacks, it never deletes
anything. Users, policies and buckets that already existed on any site end up on all of them, and the objects
written before the group are sent by a resync of every bucket.

In the console, the **Cluster** page has a **Site replication** panel to create the group, follow it and
remove it.

```console
$ nexoctl admin replicate info site-a
Site site-a (deployment 4c1e…), 128 replicated entries.
SITE             S3 ENDPOINT                              ADMIN ENDPOINT                           STATE
site-a           https://s3.a.example.com                 https://console.a.example.com            this site
site-b           https://s3.b.example.com                 https://console.b.example.com            in sync at 2026-10-22T10:15:03Z
```

## How it works

The leader of each site runs a round every few seconds with every other site. For each entry (a user, a
group, an access key, a policy, a bucket) it compares the two sides with what both had after the last round:

- changed on one side only: the change is applied on the other;
- changed on both: the most recent change wins (for an entry without a time, such as a bucket, the site with
  the smallest deployment ID);
- deleted on one side and changed on the other: the change wins, the entry comes back;
- missing on one side at the first round: it is added, never deleted.

Then every bucket gets versioning and a replication rule (`site-<name>`) to the same bucket of every other site,
with the root credentials; the objects already there are resynced. These rules show in
`nexoctl replicate ls`; your own rules are kept.

A change that cannot be applied (a bucket that is not empty on the other site, a KMS key that does not exist
there) is retried at every round and reported as pending by `admin replicate info`.

## Remove the group

```bash
nexoctl admin replicate remove site-a
```

Every site reached forgets the group and the `site-…` replication rules. Nothing is deleted: users, policies,
buckets and objects stay where they are. A site that cannot be reached keeps the group: run the command there
too.

## Limitations

- A bucket is deleted on the other sites only when it is empty there; until then the deletion is retried.
- Deleting a user does not revoke on the other sites the share links it signed.
- The temporary credentials (STS) and the console sessions are per site.
- Changing the root password on one site breaks the group: change it on every site.
- In a cluster, `admin replicate info` shows the rounds of the leader of the site.
