import type { InfoService } from '../interfaces';
import type { ServerInfo } from '../types';
import type { HttpClient } from './client';

/** InfoService over GET /info. */
export function createHttpInfoService(client: HttpClient): InfoService {
  return {
    serverInfo() {
      return client.request<ServerInfo>('/info');
    },
  };
}
