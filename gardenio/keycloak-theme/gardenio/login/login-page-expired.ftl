<#import "template.ftl" as layout>
<#-- gardenio: the parent renders two "Click here" text links; two identical link
     names are unclear to screen readers and are tiny touch targets. Same ids and
     same URLs, shown as a primary and a secondary button. -->
<@layout.registrationLayout; section>
    <#if section = "header">
        ${msg("pageExpiredTitle")}
    <#elseif section = "form">
        <div id="instruction1" class="instruction gd-expired">
            <p>${msg("pageExpiredMsg1")}</p>
            <a id="loginContinueLink" href="${url.loginAction}" class="${properties.kcButtonPrimaryClass!} ${properties.kcButtonBlockClass!}">${msg("pageExpiredContinue")}</a>
            <a id="loginRestartLink" href="${url.loginRestartFlowUrl}" class="${properties.kcButtonSecondaryClass!} ${properties.kcButtonBlockClass!}">${msg("pageExpiredRestart")}</a>
        </div>
    </#if>
</@layout.registrationLayout>
