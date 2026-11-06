import { createContext, type ReactNode, useContext, useState } from "react";

interface MuxState {
  sidebarCollapsed: boolean;
  toggleSidebar: () => void;
}

const MuxContext = createContext<MuxState>({ sidebarCollapsed: false, toggleSidebar: () => {} });

export function MuxProvider({ children }: { children: ReactNode }) {
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const toggleSidebar = () => setSidebarCollapsed((prev) => !prev);

  return (
    <MuxContext.Provider value={{ sidebarCollapsed, toggleSidebar }}>
      {children}
    </MuxContext.Provider>
  );
}

export function useMux(): MuxState {
  return useContext(MuxContext);
}
