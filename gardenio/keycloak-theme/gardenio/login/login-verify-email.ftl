<#import "template.ftl" as layout>
<#-- gardenio: the parent's only action here is a "Click here" text link in running
     text, about 17 px tall on a phone and unclear out of context (M2.4,
     gd-KiRStJd56I). Same URL, shown as a full-width secondary button that names
     what it does. The form section is the parent's (base, Keycloak 26.7.5). -->
<@layout.registrationLayout displayInfo=true; section>
    <#if section = "header">
        ${msg("emailVerifyTitle")}
    <#elseif section = "form">
        <p class="instruction">
            <#if verifyEmail??>
                ${msg("emailVerifyInstruction1",verifyEmail)}
            <#else>
                ${msg("emailVerifyInstruction4",user.email)}
            </#if>
        </p>
        <#if isAppInitiatedAction??>
            <form id="kc-verify-email-form" class="${properties.kcFormClass!}" action="${url.loginAction}" method="post">
                <div class="${properties.kcFormGroupClass!}">
                    <div id="kc-form-buttons" class="${properties.kcFormButtonsClass!}">
                        <#if verifyEmail??>
                            <input class="${properties.kcButtonClass!} ${properties.kcButtonDefaultClass!} ${properties.kcButtonLargeClass!}" type="submit" value="${msg("emailVerifyResend")}" />
                        <#else>
                            <input class="${properties.kcButtonClass!} ${properties.kcButtonPrimaryClass!} ${properties.kcButtonLargeClass!}" type="submit" value="${msg("emailVerifySend")}" />
                        </#if>
                        <button class="${properties.kcButtonClass!} ${properties.kcButtonDefaultClass!} ${properties.kcButtonLargeClass!}" type="submit" name="cancel-aia" value="true" formnovalidate>${msg("doCancel")}</button>
                    </div>
                </div>
            </form>
        </#if>
    <#elseif section = "info">
        <#if !isAppInitiatedAction??>
            <div class="instruction gd-expired">
                <p>${msg("emailVerifyInstruction2")}</p>
                <a id="gd-verify-resend" href="${url.loginAction}" class="${properties.kcButtonSecondaryClass!} ${properties.kcButtonBlockClass!}">${msg("emailVerifyResend")}</a>
            </div>
        </#if>
    </#if>
</@layout.registrationLayout>
