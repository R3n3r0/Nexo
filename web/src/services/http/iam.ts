import type { AccessKeyService, GroupService, PolicyService, UserService } from '../interfaces';
import type {
  AccessKey,
  CreatedAccessKey,
  Group,
  Policy,
  PolicyEntities,
  Simulation,
  User,
  ValidationReport,
} from '../types';
import type { HttpClient } from './client';
import { collectPages, segment } from './pages';

/** UserService over /users and /users/{accessKey}. */
export function createHttpUserService(client: HttpClient): UserService {
  return {
    list() {
      return collectPages<User>(client, '/users');
    },
    get(accessKey) {
      return client.request<User>(`/users/${segment(accessKey)}`);
    },
    create(input) {
      return client.request<User>('/users', { method: 'POST', body: input });
    },
    update(accessKey, input) {
      return client.request<User>(`/users/${segment(accessKey)}`, { method: 'PUT', body: input });
    },
    remove(accessKey) {
      return client.request<void>(`/users/${segment(accessKey)}`, { method: 'DELETE' });
    },
  };
}

/** GroupService over /groups and /groups/{name}. */
export function createHttpGroupService(client: HttpClient): GroupService {
  return {
    list() {
      return collectPages<Group>(client, '/groups');
    },
    get(name) {
      return client.request<Group>(`/groups/${segment(name)}`);
    },
    create(input) {
      return client.request<Group>('/groups', { method: 'POST', body: input });
    },
    update(name, input) {
      return client.request<Group>(`/groups/${segment(name)}`, { method: 'PUT', body: input });
    },
    remove(name) {
      return client.request<void>(`/groups/${segment(name)}`, { method: 'DELETE' });
    },
  };
}

/** PolicyService over /policies, /policies/{name} and the policy tools. */
export function createHttpPolicyService(client: HttpClient): PolicyService {
  return {
    list() {
      return collectPages<Policy>(client, '/policies');
    },
    get(name) {
      return client.request<Policy>(`/policies/${segment(name)}`);
    },
    create(input) {
      return client.request<Policy>('/policies', { method: 'POST', body: input });
    },
    save(input) {
      return client.request<Policy>(`/policies/${segment(input.name)}`, {
        method: 'PUT',
        body: { document: input.document },
      });
    },
    remove(name) {
      return client.request<void>(`/policies/${segment(name)}`, { method: 'DELETE' });
    },
    validate(input) {
      return client.request<ValidationReport>('/policies/validate', {
        method: 'POST',
        body: input,
      });
    },
    simulate(input) {
      return client.request<Simulation>('/policies/simulate', { method: 'POST', body: input });
    },
    entities(name) {
      return client.request<PolicyEntities>(`/policies/${segment(name)}/entities`);
    },
    attach(input) {
      return client.request<void>('/policies/attach', { method: 'POST', body: input });
    },
    detach(input) {
      return client.request<void>('/policies/detach', { method: 'POST', body: input });
    },
  };
}

/** AccessKeyService over /access-keys and /access-keys/{key}. */
export function createHttpAccessKeyService(client: HttpClient): AccessKeyService {
  return {
    list(filter) {
      return collectPages<AccessKey>(client, '/access-keys', { user: filter?.user });
    },
    get(accessKey) {
      return client.request<AccessKey>(`/access-keys/${segment(accessKey)}`);
    },
    create(input) {
      return client.request<CreatedAccessKey>('/access-keys', { method: 'POST', body: input });
    },
    update(accessKey, input) {
      return client.request<AccessKey>(`/access-keys/${segment(accessKey)}`, {
        method: 'PUT',
        body: input,
      });
    },
    remove(accessKey) {
      return client.request<void>(`/access-keys/${segment(accessKey)}`, { method: 'DELETE' });
    },
  };
}

// The permissions are read by the shell of the console at once: they live
// in their own module, out of the IAM services loaded with their pages.
export { createHttpPermissionService } from './permissions';
