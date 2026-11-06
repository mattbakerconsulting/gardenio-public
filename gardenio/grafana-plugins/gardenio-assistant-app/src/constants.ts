import pluginJson from './plugin.json';

export const PLUGIN_ID = pluginJson.id;
export const PLUGIN_BASE_URL = `/a/${PLUGIN_ID}`;

/**
 * Identity reported to the MCP server (`Client` name/version) and used to cache
 * the MCP client across the app. Bumping the version invalidates the cache.
 */
export const APP_NAME = 'gardenio-assistant-app';
export const APP_VERSION = '1.0.0';

export enum ROUTES {
  Chat = 'chat',
}
