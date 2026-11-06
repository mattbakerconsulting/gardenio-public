<#-- gardenio email layout. Mail clients ignore stylesheets and web fonts, so the
     look is inline styles on tables, system fonts and a text wordmark (no remote
     images). The <style> block only adds dark mode for the clients that read it
     (Apple Mail, Outlook.com, Gmail app); the inline values are the light theme
     and the safe default. The tokens are the login theme's (gardenio.css). -->
<#macro emailLayout>
<!DOCTYPE html>
<html lang="${locale.language}" dir="${(ltr)?then('ltr','rtl')}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="supported-color-schemes" content="light dark">
<title>${msg("gdEmailBrand")}</title>
<style>
  @media (prefers-color-scheme: dark) {
    body, .gd-page { background: #0b0f14 !important; color: #d6e0ec !important; }
    .gd-card { background: #11161d !important; border-color: #1e2631 !important; color: #d6e0ec !important; }
    .gd-mark, .gd-card p, .gd-card td { color: #d6e0ec !important; }
    .gd-card .gd-muted, .gd-foot { color: #8b99ac !important; }
    .gd-link { color: #6ee7b7 !important; }
    .gd-btn { background: #6ee7b7 !important; color: #0b0f14 !important; }
  }
  @media (max-width: 480px) {
    .gd-card { padding: 20px 16px !important; }
  }
</style>
</head>
<body style="margin:0;padding:0;background:#f3f4f6;color:#111827;">
<table role="presentation" class="gd-page" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f3f4f6;">
  <tr>
    <td align="center" style="padding:24px 12px;">
      <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;">
        <tr>
          <td class="gd-mark" style="padding:0 0 16px 0;font-family:-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:26px;line-height:30px;font-weight:700;letter-spacing:-0.5px;color:#111827;">${msg("gdEmailBrand")}</td>
        </tr>
        <tr>
          <td class="gd-card" style="background:#ffffff;border:1px solid #d1d5db;border-radius:4px;padding:28px 24px;font-family:-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:16px;line-height:24px;color:#111827;">
            <#nested>
          </td>
        </tr>
        <tr>
          <td class="gd-foot" style="padding:16px 0 0 0;font-family:-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:13px;line-height:20px;color:#4b5563;">${msg("gdEmailFooter")}</td>
        </tr>
      </table>
    </td>
  </tr>
</table>
</body>
</html>
</#macro>

<#-- The message's call to action: a button, then the bare link for clients that
     strip buttons, then how long it lasts. `url` is escaped by the template's
     HTML output format, like every other value here. -->
<#macro emailAction url label expires>
<p style="margin:0 0 20px 0;"><a class="gd-btn" href="${url}" style="display:inline-block;background:#1d4ed8;color:#ffffff;text-decoration:none;font-weight:600;padding:12px 20px;border-radius:4px;">${label}</a></p>
<p class="gd-muted" style="margin:0 0 4px 0;font-size:13px;line-height:20px;color:#4b5563;">${msg("gdEmailLinkFallback")}</p>
<p style="margin:0 0 20px 0;font-size:13px;line-height:20px;word-break:break-all;"><a class="gd-link" href="${url}" style="color:#1d4ed8;">${url}</a></p>
<p class="gd-muted" style="margin:0;font-size:14px;line-height:22px;color:#4b5563;">${expires}</p>
</#macro>
