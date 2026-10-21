import {
  Alert,
  Button,
  Center,
  Container,
  Divider,
  Paper,
  PasswordInput,
  SegmentedControl,
  Stack,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { useQuery } from '@tanstack/react-query';
import {
  IconAlertCircle,
  IconClockExclamation,
  IconInfoCircle,
  IconLogin2,
} from '@tabler/icons-react';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, useLocation, useNavigate, useSearchParams } from 'react-router';

import { redirectTargetFromState, sessionExpiredFromState } from '@/auth/redirect';
import { useSession } from '@/auth/SessionProvider';
import { LegalNotice } from '@/components/LegalNotice';
import { isApiError, useServices } from '@/services';

import {
  isLoginValid,
  minAccessKeyLength,
  minSecretKeyLength,
  validateLogin,
  type LoginFieldError,
} from './loginValidation';

export interface LoginPageProps {
  /** Shown as a hint when the console runs against the mock services. */
  demoCredentials?: { accessKey: string; secretKey: string };
}

/** Errors of the OpenID Connect callback (GET /auth/openid/callback). */
const ssoErrors = ['sso_denied', 'sso_unavailable', 'sso_failed'] as const;
type SSOError = (typeof ssoErrors)[number];

function isSSOError(value: string | null): value is SSOError {
  return ssoErrors.some((code) => code === value);
}

type LoginMode = 'accessKey' | 'ldap';

export function LoginPage({ demoCredentials }: LoginPageProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const { session: sessions } = useServices();
  const { status, login, loginLDAP } = useSession();
  // The providers only add options: the access keys always work, so a
  // failure to read them leaves the plain form.
  const providers = useQuery({
    queryKey: ['auth', 'providers'],
    queryFn: () => sessions.providers(),
    retry: false,
    staleTime: Infinity,
  });

  const [mode, setMode] = useState<LoginMode>('accessKey');
  const [accessKey, setAccessKey] = useState('');
  const [secretKey, setSecretKey] = useState('');
  const [touched, setTouched] = useState({ accessKey: false, secretKey: false });
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const ssoError = searchParams.get('error');

  const target = redirectTargetFromState(location.state);
  const expired = sessionExpiredFromState(location.state);
  if (status === 'authenticated') {
    return <Navigate to={target} replace />;
  }

  const ldap = mode === 'ldap';
  const validation = ldap ? {} : validateLogin(accessKey, secretKey);
  const ldapValid = accessKey.trim() !== '' && secretKey !== '';
  const canSubmit = (ldap ? ldapValid : isLoginValid(validation)) && !submitting;

  const fieldError = (code: LoginFieldError | undefined, isTouched: boolean) => {
    if (code === undefined || !isTouched) {
      return undefined;
    }
    return t(`login.validation.${code}`, {
      min: code.startsWith('accessKey') ? minAccessKeyLength : minSecretKeyLength,
    });
  };
  const requiredError = (value: string, isTouched: boolean, key: 'username' | 'password') =>
    ldap && isTouched && value === '' ? t(`login.${key}Required`) : undefined;

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setTouched({ accessKey: true, secretKey: true });
    if (!canSubmit) {
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      if (ldap) {
        await loginLDAP(accessKey.trim(), secretKey);
      } else {
        await login(accessKey.trim(), secretKey);
      }
      await navigate(target, { replace: true });
    } catch (cause) {
      if (isApiError(cause) && (cause.isUnauthorized || cause.isForbidden)) {
        setError(t(ldap ? 'login.invalidLDAPCredentials' : 'login.invalidCredentials'));
      } else if (ldap && isApiError(cause) && cause.status === 503) {
        setError(t('login.ldapUnavailable'));
      } else if (isApiError(cause) && cause.isTooManyRequests) {
        setError(t('login.tooManyAttempts'));
      } else {
        const message = cause instanceof Error ? cause.message : t('common.unknownError');
        setError(t('login.failed', { message }));
      }
      setSubmitting(false);
    }
  };

  return (
    <Center mih="100vh" p="md">
      <Container size={420} w="100%">
        <Stack gap="lg">
          <Stack gap={4} align="center">
            <Title order={1}>{t('login.title')}</Title>
            <Text c="dimmed" size="sm">
              {t(ldap ? 'login.ldapSubtitle' : 'login.subtitle')}
            </Text>
          </Stack>

          <Paper withBorder shadow="sm" p="xl" radius="md">
            {isSSOError(ssoError) && !error ? (
              <Alert
                color="red"
                variant="light"
                icon={<IconAlertCircle size={18} />}
                role="alert"
                mb="md"
              >
                {t(`login.ssoErrors.${ssoError}`)}
              </Alert>
            ) : null}
            {providers.data?.openid === true ? (
              <>
                <Button
                  component="a"
                  href={sessions.openIDLoginURL()}
                  fullWidth
                  variant="default"
                  leftSection={<IconLogin2 size={18} />}
                >
                  {t('login.sso', { name: providers.data.openidName ?? 'SSO' })}
                </Button>
                <Divider my="md" label={t('login.or')} labelPosition="center" />
              </>
            ) : null}
            <form onSubmit={handleSubmit} noValidate aria-label={t('login.title')}>
              <Stack gap="md">
                {providers.data?.ldap === true ? (
                  <SegmentedControl
                    fullWidth
                    aria-label={t('login.modeLabel')}
                    value={mode}
                    onChange={(value) => {
                      setMode(value === 'ldap' ? 'ldap' : 'accessKey');
                      setError(null);
                      setTouched({ accessKey: false, secretKey: false });
                    }}
                    data={[
                      { value: 'accessKey', label: t('login.modeAccessKey') },
                      { value: 'ldap', label: t('login.modeLDAP') },
                    ]}
                    disabled={submitting}
                  />
                ) : null}
                {demoCredentials ? (
                  <Alert variant="light" icon={<IconInfoCircle size={18} />}>
                    {t('login.demoHint', demoCredentials)}
                  </Alert>
                ) : null}
                {expired && !error ? (
                  <Alert color="yellow" variant="light" icon={<IconClockExclamation size={18} />}>
                    {t('login.sessionExpired')}
                  </Alert>
                ) : null}
                {error ? (
                  <Alert
                    color="red"
                    variant="light"
                    icon={<IconAlertCircle size={18} />}
                    role="alert"
                  >
                    {error}
                  </Alert>
                ) : null}
                <TextInput
                  label={t(ldap ? 'login.username' : 'login.accessKey')}
                  placeholder={t(ldap ? 'login.usernamePlaceholder' : 'login.accessKeyPlaceholder')}
                  name={ldap ? 'username' : 'accessKey'}
                  autoComplete="username"
                  required
                  value={accessKey}
                  onChange={(event) => {
                    setAccessKey(event.currentTarget.value);
                  }}
                  onBlur={() => {
                    setTouched((prev) => ({ ...prev, accessKey: true }));
                  }}
                  error={
                    fieldError(validation.accessKey, touched.accessKey) ??
                    requiredError(accessKey.trim(), touched.accessKey, 'username')
                  }
                  disabled={submitting}
                  data-autofocus
                />
                <PasswordInput
                  label={t(ldap ? 'login.password' : 'login.secretKey')}
                  placeholder={t(ldap ? 'login.passwordPlaceholder' : 'login.secretKeyPlaceholder')}
                  name={ldap ? 'password' : 'secretKey'}
                  autoComplete="current-password"
                  required
                  value={secretKey}
                  onChange={(event) => {
                    setSecretKey(event.currentTarget.value);
                  }}
                  onBlur={() => {
                    setTouched((prev) => ({ ...prev, secretKey: true }));
                  }}
                  error={
                    fieldError(validation.secretKey, touched.secretKey) ??
                    requiredError(secretKey, touched.secretKey, 'password')
                  }
                  disabled={submitting}
                />
                <Button type="submit" fullWidth disabled={!canSubmit} loading={submitting}>
                  {submitting ? t('login.submitting') : t('login.submit')}
                </Button>
              </Stack>
            </form>
          </Paper>
          <LegalNotice />
        </Stack>
      </Container>
    </Center>
  );
}
