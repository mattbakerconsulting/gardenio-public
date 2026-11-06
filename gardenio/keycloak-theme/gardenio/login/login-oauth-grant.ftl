<#import "template.ftl" as layout>
<#import "buttons.ftl" as buttons>
<#-- gardenio: the parent's page with plain wording. "Yes" / "No" are not an answer
     to "Do you grant these access privileges?"; the buttons say Allow / Do not
     allow, and the request line names the app. Ids, names and the hidden code
     are the parent's (kc-login = accept, kc-cancel = cancel). -->
<@layout.registrationLayout bodyClass="oauth"; section>
    <#if section = "header">
        <#if client.attributes.logoUri??>
            <img src="${client.attributes.logoUri}" alt=""/>
        </#if>
        <#if client.name?has_content>
            ${msg("oauthGrantTitle",advancedMsg(client.name))}
        <#else>
            ${msg("oauthGrantTitle",client.clientId)}
        </#if>
    <#elseif section = "form">
        <div id="kc-oauth" class="content-area">
            <p class="instruction">
                <#if client.name?has_content>
                    ${msg("oauthGrantRequest",advancedMsg(client.name))}
                <#else>
                    ${msg("oauthGrantRequest",client.clientId)}
                </#if>
            </p>
            <ul class="${properties.kcListClass!}">
                <#if oauth.clientScopesRequested??>
                    <#list oauth.clientScopesRequested as clientScope>
                        <li>
                            <span><#if !clientScope.parameterizedScopeParameter??>
                                        ${advancedMsg(clientScope.consentScreenText)}
                                    <#else>
                                        ${advancedMsg(clientScope.consentScreenText, clientScope.parameterizedScopeParameter)}
                                </#if>
                            </span>
                        </li>
                    </#list>
                </#if>
            </ul>
            <#if client.attributes.policyUri?? || client.attributes.tosUri??>
                <p class="instruction">
                    <#if client.name?has_content>
                        ${msg("oauthGrantInformation",advancedMsg(client.name))}
                    <#else>
                        ${msg("oauthGrantInformation",client.clientId)}
                    </#if>
                    <#if client.attributes.tosUri??>
                        ${msg("oauthGrantReview")}
                        <a href="${client.attributes.tosUri}" target="_blank" rel="noopener">${msg("oauthGrantTos")}</a>
                    </#if>
                    <#if client.attributes.policyUri??>
                        ${msg("oauthGrantReview")}
                        <a href="${client.attributes.policyUri}" target="_blank" rel="noopener">${msg("oauthGrantPolicy")}</a>
                    </#if>
                </p>
            </#if>

            <form class="${properties.kcFormClass} ${properties.kcMarginTopClass!}" action="${url.oauthAction}" method="POST">
                <input type="hidden" name="code" value="${oauth.code}">
                <@buttons.actionGroup>
                    <@buttons.button id="kc-login" name="accept" label="gdGrantAllow"/>
                    <@buttons.button id="kc-cancel" name="cancel" label="gdGrantDeny" type="secondary"/>
                </@buttons.actionGroup>
            </form>
        </div>
    </#if>
</@layout.registrationLayout>
