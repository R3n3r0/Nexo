import type { AccessKeyService, GroupService, PolicyService, UserService } from '../interfaces';
import type { AccessKey, CreatedAccessKey, Group, Policy, User } from '../types';
import type { HttpClient } from './client';

/** UserService over /users and /users/{accessKey}. */
export function createHttpUserService(client: HttpClient): UserService {
  return {
    list() {
      return client.request<User[]>('/users');
    },
    create(input) {
      return client.request<User>('/users', { method: 'POST', body: input });
    },
    remove(accessKey) {
      return client.request<void>(`/users/${encodeURIComponent(accessKey)}`, { method: 'DELETE' });
    },
  };
}

/** GroupService over /groups and /groups/{name}. */
export function createHttpGroupService(client: HttpClient): GroupService {
  return {
    list() {
      return client.request<Group[]>('/groups');
    },
    create(input) {
      return client.request<Group>('/groups', { method: 'POST', body: input });
    },
    remove(name) {
      return client.request<void>(`/groups/${encodeURIComponent(name)}`, { method: 'DELETE' });
    },
  };
}

/** PolicyService over /policies and /policies/{name}. */
export function createHttpPolicyService(client: HttpClient): PolicyService {
  return {
    list() {
      return client.request<Policy[]>('/policies');
    },
    get(name) {
      return client.request<Policy>(`/policies/${encodeURIComponent(name)}`);
    },
    save(input) {
      return client.request<Policy>(`/policies/${encodeURIComponent(input.name)}`, {
        method: 'PUT',
        body: { document: input.document },
      });
    },
    remove(name) {
      return client.request<void>(`/policies/${encodeURIComponent(name)}`, { method: 'DELETE' });
    },
  };
}

/** AccessKeyService over /access-keys and /access-keys/{key}. */
export function createHttpAccessKeyService(client: HttpClient): AccessKeyService {
  return {
    list() {
      return client.request<AccessKey[]>('/access-keys');
    },
    create(input) {
      return client.request<CreatedAccessKey>('/access-keys', { method: 'POST', body: input });
    },
    remove(accessKey) {
      return client.request<void>(`/access-keys/${encodeURIComponent(accessKey)}`, {
        method: 'DELETE',
      });
    },
  };
}
