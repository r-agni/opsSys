<#import "template.ftl" as layout>
<@layout.registrationLayout; section>
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
                    <p>Create your account</p>
                </div>

                <#if message?has_content && (message.type != 'warning' || !isAppInitiatedAction??)>
                    <div class="alert alert-${message.type}">
                        ${kcSanitize(message.summary)?no_esc}
                    </div>
                </#if>

                <form id="kc-register-form" action="${url.registrationAction}" method="post">

                    <div class="form-group">
                        <label for="firstName">${msg("firstName")}</label>
                        <input type="text" id="firstName" name="firstName" value="${(register.formData.firstName!'')}" placeholder="First name" />
                    </div>

                    <div class="form-group">
                        <label for="lastName">${msg("lastName")}</label>
                        <input type="text" id="lastName" name="lastName" value="${(register.formData.lastName!'')}" placeholder="Last name" />
                    </div>

                    <div class="form-group">
                        <label for="email">${msg("email")}</label>
                        <input type="email" id="email" name="email" value="${(register.formData.email!'')}" autocomplete="email" placeholder="you@example.com" />
                    </div>

                    <#if !realm.registrationEmailAsUsername>
                        <div class="form-group">
                            <label for="username">${msg("username")}</label>
                            <input type="text" id="username" name="username" value="${(register.formData.username!'')}" autocomplete="username" placeholder="Choose a username" />
                        </div>
                    </#if>

                    <#if passwordRequired??>
                        <div class="form-group">
                            <label for="password">${msg("password")}</label>
                            <input type="password" id="password" name="password" autocomplete="new-password" placeholder="Create a password" />
                        </div>

                        <div class="form-group">
                            <label for="password-confirm">${msg("passwordConfirm")}</label>
                            <input type="password" id="password-confirm" name="password-confirm" placeholder="Confirm password" />
                        </div>
                    </#if>

                    <#if recaptchaRequired??>
                        <div class="form-group">
                            <div class="g-recaptcha" data-size="compact" data-sitekey="${recaptchaSiteKey}"></div>
                        </div>
                    </#if>

                    <input type="submit" id="kc-register" value="${msg("doRegister")}" />

                    <div id="kc-form-options" style="text-align:center; margin-top:20px; padding-top:16px; border-top:1px solid #2a3040;">
                        <span style="color:#8899aa; font-size:12px; display:block; margin-bottom:8px;">Already have an account?</span>
                        <a href="${url.loginUrl}">Sign in</a>
                    </div>

                    <div class="ss-hint">Secured by Keycloak OIDC &middot; PKCE flow</div>
                </form>
            </div>
        </div>
    </#if>
</@layout.registrationLayout>
