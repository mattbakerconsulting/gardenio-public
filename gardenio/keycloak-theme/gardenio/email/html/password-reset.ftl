<#import "template.ftl" as layout>
<@layout.emailLayout>
<p style="margin:0 0 12px 0;font-size:20px;line-height:28px;font-weight:600;">${msg("gdEmailResetTitle")}</p>
<p style="margin:0 0 20px 0;">${msg("gdEmailResetIntro")}</p>
<@layout.emailAction url=link label=msg("gdEmailResetButton") expires=msg("gdEmailExpires", linkExpirationFormatter(linkExpiration)) />
<p class="gd-muted" style="margin:20px 0 0 0;font-size:14px;line-height:22px;color:#4b5563;">${msg("gdEmailResetIgnore")}</p>
</@layout.emailLayout>
