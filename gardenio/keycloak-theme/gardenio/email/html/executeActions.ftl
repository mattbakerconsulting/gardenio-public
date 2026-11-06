<#outputformat "plainText">
<#assign requiredActionsText><#if requiredActions??><#list requiredActions><#items as reqActionItem>${msg("requiredAction.${reqActionItem}")}<#sep>, </#sep></#items></#list></#if></#assign>
</#outputformat>

<#import "template.ftl" as layout>
<@layout.emailLayout>
<p style="margin:0 0 12px 0;font-size:20px;line-height:28px;font-weight:600;">${msg("gdEmailActionsTitle")}</p>
<p style="margin:0 0 20px 0;">${msg("gdEmailActionsIntro", requiredActionsText)}</p>
<@layout.emailAction url=link label=msg("gdEmailActionsButton") expires=msg("gdEmailExpires", linkExpirationFormatter(linkExpiration)) />
<p class="gd-muted" style="margin:20px 0 0 0;font-size:14px;line-height:22px;color:#4b5563;">${msg("gdEmailActionsIgnore")}</p>
</@layout.emailLayout>
