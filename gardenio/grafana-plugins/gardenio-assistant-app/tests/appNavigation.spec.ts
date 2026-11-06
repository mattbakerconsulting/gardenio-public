import { test, expect } from './fixtures';
import { ROUTES } from '../src/constants';

test.describe('navigating the gardenio assistant', () => {
  test('chat page should render', async ({ gotoPage, page }) => {
    await gotoPage(`/${ROUTES.Chat}`);
    // The chat page container renders regardless of whether the LLM/MCP backend
    // is reachable (it degrades to a warning/error alert), so assert on the
    // gardenio branding which is always present.
    await expect(page.getByText('Gardenio Assistant')).toBeVisible();
  });

  test('default route falls back to the chat page', async ({ gotoPage, page }) => {
    await gotoPage();
    await expect(page.getByText('Gardenio Assistant')).toBeVisible();
  });
});
