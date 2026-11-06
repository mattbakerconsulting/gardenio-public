
## React conventions

- Use `key` to trigger re-renders instead of clearing state.
- Prefer direct const assignment with array filtering and mapping, or dictionary/lookup construction.
- Perform state updates in the event handler, and NOT in an effect.
- Prefer `createContext` to manage state shared across sibling components.
- Prefer `use` over `useContext` to access context.
- AVOID `useCallback`, it likely isn't need in React 19.
- AVOID prop-drilling and excessively long parameter list.
- AVOID passing `state/setState` calls to children, prefer actions instead.
- AVOID and almost never use `useEffect`, you probably don't need it.
  - Don't call `setState` in a useEffect, adjust it during render instead.
  - For fire-and-forget initialization (no cleanup), use `useInit` from `@/hooks/utils`.
  - For persistent state, use `useLocalStorage` from `@/hooks/utils`.

