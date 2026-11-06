import type { Tool as MCPTool } from '@modelcontextprotocol/sdk/types.js';
import {
  curateReadOnlyTools,
  curateTools,
  isToolAllowed,
  isWriteShaped,
  READ_ONLY_TOOLS,
  WRITE_TOOLS,
} from './readOnlyTools';

const mkTool = (name: string): MCPTool => ({
  name,
  description: name,
  inputSchema: { type: 'object' },
});

describe('readOnlyTools', () => {
  describe('isWriteShaped', () => {
    it.each([
      'update_dashboard',
      'create_incident',
      'delete_snapshot',
      'add_activity_to_incident',
      'create_annotation',
      'update_annotation',
      'create_folder',
    ])('flags write-shaped tool %s', (name) => {
      expect(isWriteShaped(name)).toBe(true);
    });

    it.each(['query_prometheus', 'search_dashboards', 'list_datasources', 'get_dashboard_by_uid'])(
      'does not flag read tool %s',
      (name) => {
        expect(isWriteShaped(name)).toBe(false);
      }
    );

    it('flags every vetted write tool (so the gate, not the heuristic, must admit them)', () => {
      // The whole point of the write gate is that our vetted write tools ARE
      // write-shaped; isToolAllowed admits them via the explicit allow-list.
      for (const name of WRITE_TOOLS) {
        expect(isWriteShaped(name)).toBe(true);
      }
    });
  });

  describe('WRITE_TOOLS allow-list', () => {
    it('is bounded to dashboard + folder create/update ONLY', () => {
      // Hard envelope assertion (ka-0000005589): no alerting/annotations/
      // datasource-write/admin/incident/oncall tool may ever be on this list.
      expect([...WRITE_TOOLS].sort()).toEqual(['create_folder', 'update_dashboard']);
    });

    it('shares no names with the read-only set', () => {
      const readSet = new Set(READ_ONLY_TOOLS);
      for (const name of WRITE_TOOLS) {
        expect(readSet.has(name)).toBe(false);
      }
    });

    it('contains no obviously-destructive verbs (delete/remove)', () => {
      for (const name of WRITE_TOOLS) {
        expect(name).not.toMatch(/delete|remove/i);
      }
    });
  });

  describe('isToolAllowed (writes OFF — default)', () => {
    it('allows curated read-only tools', () => {
      expect(isToolAllowed('query_prometheus')).toBe(true);
      expect(isToolAllowed('search_dashboards')).toBe(true);
      expect(isToolAllowed('list_datasources')).toBe(true);
    });

    it('rejects the vetted write tools when writes are disabled', () => {
      for (const name of WRITE_TOOLS) {
        expect(isToolAllowed(name)).toBe(false);
        expect(isToolAllowed(name, false)).toBe(false);
      }
    });

    it('rejects write tools even if not on the deny list explicitly', () => {
      expect(isToolAllowed('update_dashboard')).toBe(false);
      expect(isToolAllowed('create_incident')).toBe(false);
    });

    it('rejects unknown tools not on the allow-list', () => {
      expect(isToolAllowed('some_future_tool')).toBe(false);
      expect(isToolAllowed('run_panel_query')).toBe(false);
    });

    it('omits alerting management tools (write-capable via operation param)', () => {
      expect(isToolAllowed('alerting_manage_rules')).toBe(false);
      expect(isToolAllowed('alerting_manage_routing')).toBe(false);
    });
  });

  describe('isToolAllowed (writes ON)', () => {
    it('still allows every read-only tool', () => {
      for (const name of READ_ONLY_TOOLS) {
        expect(isToolAllowed(name, true)).toBe(true);
      }
    });

    it('admits the vetted write tools', () => {
      expect(isToolAllowed('update_dashboard', true)).toBe(true);
      expect(isToolAllowed('create_folder', true)).toBe(true);
    });

    it('does NOT admit any other write-shaped tool (only the vetted names)', () => {
      // The gate widens by EXACTLY WRITE_TOOLS — nothing else write-shaped slips
      // in just because writes are enabled.
      for (const name of [
        'create_incident',
        'delete_snapshot',
        'create_annotation',
        'update_annotation',
        'alerting_manage_rules',
        'delete_dashboard',
        'update_datasource',
        'create_user',
      ]) {
        expect(isToolAllowed(name, true)).toBe(false);
      }
    });
  });

  describe('curateReadOnlyTools (back-compat: read-only only)', () => {
    it('filters a mixed list down to read-only tools', () => {
      const input = [
        mkTool('query_prometheus'),
        mkTool('update_dashboard'),
        mkTool('search_dashboards'),
        mkTool('create_incident'),
        mkTool('delete_snapshot'),
        mkTool('list_datasources'),
      ];
      const out = curateReadOnlyTools(input).map((t) => t.name);
      expect(out).toEqual(['query_prometheus', 'search_dashboards', 'list_datasources']);
    });

    it('never returns a write-shaped tool from the full allow-list', () => {
      // Defense-in-depth invariant: no curated read-only tool name is write-shaped.
      for (const name of READ_ONLY_TOOLS) {
        expect(isWriteShaped(name)).toBe(false);
      }
    });

    it('returns an empty list when given only write tools', () => {
      const input = [mkTool('update_dashboard'), mkTool('create_annotation')];
      expect(curateReadOnlyTools(input)).toEqual([]);
    });
  });

  describe('curateTools', () => {
    // The realistic advertised surface: read tools + the C7 server's write tools
    // + unvetted write tools the server might add in a future version.
    const advertised = [
      mkTool('query_prometheus'),
      mkTool('search_dashboards'),
      mkTool('list_datasources'),
      mkTool('update_dashboard'),
      mkTool('create_folder'),
      mkTool('create_incident'), // unvetted write — must NEVER survive
      mkTool('delete_dashboard'), // unvetted write — must NEVER survive
    ];

    it('writes OFF -> advertised surface == read-only set (no create/update)', () => {
      const out = curateTools(advertised, false).map((t) => t.name);
      expect(out).toEqual(['query_prometheus', 'search_dashboards', 'list_datasources']);
      // explicit: neither vetted write tool is exposed when writes are off.
      expect(out).not.toContain('update_dashboard');
      expect(out).not.toContain('create_folder');
    });

    it('writes ON -> read-only set + vetted write names AND nothing else write-shaped', () => {
      const out = curateTools(advertised, true).map((t) => t.name);
      expect(out).toEqual([
        'query_prometheus',
        'search_dashboards',
        'list_datasources',
        'update_dashboard',
        'create_folder',
      ]);
      // The unvetted write tools are netted by the heuristic even with writes on.
      expect(out).not.toContain('create_incident');
      expect(out).not.toContain('delete_dashboard');
    });

    it('writes ON exposes EXACTLY the read-only set plus WRITE_TOOLS, no more', () => {
      // Feed the union of every read + every write tool name; the curated surface
      // must equal READ_ONLY_TOOLS ∪ WRITE_TOOLS and contain nothing extra.
      const everything = [...READ_ONLY_TOOLS, ...WRITE_TOOLS].map(mkTool);
      const out = new Set(curateTools(everything, true).map((t) => t.name));
      expect(out).toEqual(new Set([...READ_ONLY_TOOLS, ...WRITE_TOOLS]));
    });

    it('defaults to read-only when enableWrites is omitted', () => {
      const out = curateTools(advertised).map((t) => t.name);
      expect(out).toEqual(['query_prometheus', 'search_dashboards', 'list_datasources']);
    });
  });
});
