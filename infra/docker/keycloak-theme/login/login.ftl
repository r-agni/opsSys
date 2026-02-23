<#import "template.ftl" as layout>
<@layout.registrationLayout displayInfo=social.displayInfo; section>
    <#if section = "header">
    <#elseif section = "form">
        <div id="kc-form">
            <div id="kc-form-wrapper">
                <div class="ss-logo">
                    <svg width="44" height="44" viewBox="0 0 44 44" fill="none">
                        <rect x="2" y="2" width="18" height="18" rx="4" fill="#e04040"/>
                        <rect x="24" y="2" width="18" height="18" rx="4" fill="#e04040" opacity="0.55"/>
                        <rect x="2" y="24" width="18" height="18" rx="4" fill="#e04040" opacity="0.55"/>
                        <rect x="24" y="24" width="18" height="18" rx="4" fill="#e04040" opacity="0.2"/>
                    </svg>
                    <h1>SystemScale</h1>
                    <p>Operator Dashboard</p>
                </div>

                <#if message?has_content && (message.type != 'warning' || !isAppInitiatedAction??)>
                    <div class="alert alert-${message.type}">
                        ${kcSanitize(message.summary)?no_esc}
                    </div>
                </#if>

                <form id="kc-form-login" onsubmit="login.disabled = true; return true;" action="${url.loginAction}" method="post">
                    <div class="form-group">
                        <label for="username">
                            <#if !realm.loginWithEmailAllowed>${msg("username")}<#elseif !realm.registrationEmailAsUsername>${msg("usernameOrEmail")}<#else>${msg("email")}</#if>
                        </label>
                        <input tabindex="1" id="username" name="username" value="${(login.username!'')}" type="text" autofocus autocomplete="off" placeholder="you@example.com" />
                    </div>

                    <div class="form-group">
                        <label for="password">${msg("password")}</label>
                        <input tabindex="2" id="password" name="password" type="password" autocomplete="off" placeholder="Enter your password" />
                    </div>

                    <div class="form-group" style="display:flex; align-items:center; justify-content:space-between;">
                        <#if realm.rememberMe && !usernameEditDisabled??>
                            <div class="checkbox">
                                <label>
                                    <#if login.rememberMe??>
                                        <input tabindex="3" id="rememberMe" name="rememberMe" type="checkbox" checked> ${msg("rememberMe")}
                                    <#else>
                                        <input tabindex="3" id="rememberMe" name="rememberMe" type="checkbox"> ${msg("rememberMe")}
                                    </#if>
                                </label>
                            </div>
                        </#if>
                        <#if realm.resetPasswordAllowed>
                            <div class="kc-form-options-wrapper">
                                <a tabindex="5" href="${url.loginResetCredentialsUrl}">${msg("doForgotPassword")}</a>
                            </div>
                        </#if>
                    </div>

                    <input tabindex="4" name="login" id="kc-login" type="submit" value="${msg("doLogIn")}" />
                </form>

                <#if realm.password && realm.registrationAllowed && !registrationDisabled??>
                    <div id="kc-registration">
                        <span>${msg("noAccount")}</span>
                        <a tabindex="6" href="${url.registrationUrl}">${msg("doRegister")}</a>
                    </div>
                </#if>

                <div class="ss-hint">Secured by Keycloak OIDC &middot; PKCE flow</div>
            </div>
        </div>
    </#if>
</@layout.registrationLayout>
