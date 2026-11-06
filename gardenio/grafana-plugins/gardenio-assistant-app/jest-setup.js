// Jest setup provided by Grafana scaffolding
import './.config/jest-setup';

// MCP functionality (@grafana/llm/mcp) relies on web-stream + encoder globals
// that jsdom does not provide. Polyfill them so tests importing the MCP client
// don't throw at module-eval time.
import { TransformStream } from 'node:stream/web';
import { TextEncoder } from 'util';

global.TextEncoder = global.TextEncoder ?? TextEncoder;
global.TransformStream = global.TransformStream ?? TransformStream;
