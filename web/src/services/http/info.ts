import type { InfoService } from '../interfaces';
import type { DataUsage, ServerInfo } from '../types';
import type { HttpClient } from './client';

/** InfoService over GET /info and GET /usage. */
export function createHttpInfoService(client: HttpClient): InfoService {
  return {
    serverInfo() {
      return client.request<ServerInfo>('/info');
    },
    usage() {
      return client.request<DataUsage>('/usage');
    },
  };
}
