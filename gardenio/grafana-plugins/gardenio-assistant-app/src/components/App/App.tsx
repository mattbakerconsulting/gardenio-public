import React from 'react';
import { Route, Routes } from 'react-router-dom';
import { AppRootProps } from '@grafana/data';
import { ROUTES } from '../../constants';
import type { GardenioAssistantJsonData } from '../../types';

const ChatPage = React.lazy(() => import('../../pages/ChatPage'));

function App(props: AppRootProps<GardenioAssistantJsonData>) {
  // Per-env opt-in (provisioned jsonData, default false) that lets the assistant
  // create/update dashboards + folders (C8, ka-0000005589). Read once here from
  // the plugin meta and threaded into the chat surface; absent/false keeps the
  // hard read-only posture.
  const enableDashboardWrites = props.meta.jsonData?.enableDashboardWrites ?? false;
  return (
    <Routes>
      <Route path={ROUTES.Chat} element={<ChatPage enableDashboardWrites={enableDashboardWrites} />} />
      {/* Default to the chat page for any unmatched sub-path. */}
      <Route path="*" element={<ChatPage enableDashboardWrites={enableDashboardWrites} />} />
    </Routes>
  );
}

export default App;
