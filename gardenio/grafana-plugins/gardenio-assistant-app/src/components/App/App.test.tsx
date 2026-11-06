import React from 'react';
import { MemoryRouter } from 'react-router-dom';
import { AppRootProps, PluginType } from '@grafana/data';
import { render, waitFor } from '@testing-library/react';
import App from './App';
import { testIds } from '../testIds';

// Mock @grafana/llm so the MCP provider/client don't try to reach the backend.
jest.mock('@grafana/llm', () => {
  const React_ = require('react');
  return {
    llm: {
      enabled: jest.fn().mockResolvedValue(false),
      Model: { BASE: 'base', LARGE: 'large' },
      streamChatCompletions: jest.fn(),
      accumulateContent: jest.fn(),
      accumulateToolCalls: jest.fn(),
      isToolCallsMessage: jest.fn().mockReturnValue(false),
    },
    mcp: {
      MCPClientProvider: ({ children }: { children: React.ReactNode }) => React_.createElement(React_.Fragment, null, children),
      useMCPClient: () => ({ client: { listTools: jest.fn().mockResolvedValue({ tools: [] }) }, enabled: true }),
      convertToolsToOpenAI: (t: unknown) => t,
    },
  };
});

describe('Components/App', () => {
  let props: AppRootProps;

  beforeEach(() => {
    jest.clearAllMocks();

    props = {
      basename: 'a/gardenio-assistant-app',
      meta: {
        id: 'gardenio-assistant-app',
        name: 'Gardenio Assistant',
        type: PluginType.app,
        enabled: true,
        jsonData: {},
      },
      query: {},
      path: '',
      onNavChanged: jest.fn(),
    } as unknown as AppRootProps;
  });

  test('renders the chat page without an error', async () => {
    const { queryByTestId } = render(
      <MemoryRouter>
        <App {...props} />
      </MemoryRouter>
    );

    // The chat page is lazy loaded; wait for its container to appear.
    await waitFor(() => expect(queryByTestId(testIds.chatPage.container)).toBeInTheDocument(), { timeout: 3000 });
  });
});
