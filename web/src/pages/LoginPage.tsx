import {
  Alert,
  Button,
  Center,
  Container,
  Paper,
  PasswordInput,
  Stack,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconAlertCircle, IconClockExclamation, IconInfoCircle } from '@tabler/icons-react';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, useLocation, useNavigate } from 'react-router';

import { redirectTargetFromState, sessionExpiredFromState } from '@/auth/redirect';
import { useSession } from '@/auth/SessionProvider';
import { isApiError } from '@/services';

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

export function LoginPage({ demoCredentials }: LoginPageProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const location = useLocation();
  const { status, login } = useSession();

  const [accessKey, setAccessKey] = useState('');
  const [secretKey, setSecretKey] = useState('');
  const [touched, setTouched] = useState({ accessKey: false, secretKey: false });
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const target = redirectTargetFromState(location.state);
  const expired = sessionExpiredFromState(location.state);
  if (status === 'authenticated') {
    return <Navigate to={target} replace />;
  }

  const validation = validateLogin(accessKey, secretKey);
  const canSubmit = isLoginValid(validation) && !submitting;

  const fieldError = (code: LoginFieldError | undefined, isTouched: boolean) => {
    if (code === undefined || !isTouched) {
      return undefined;
    }
    return t(`login.validation.${code}`, {
      min: code.startsWith('accessKey') ? minAccessKeyLength : minSecretKeyLength,
    });
  };

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setTouched({ accessKey: true, secretKey: true });
    if (!canSubmit) {
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await login(accessKey.trim(), secretKey);
      await navigate(target, { replace: true });
    } catch (cause) {
      if (isApiError(cause) && (cause.isUnauthorized || cause.isForbidden)) {
        setError(t('login.invalidCredentials'));
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
              {t('login.subtitle')}
            </Text>
          </Stack>

          <Paper withBorder shadow="sm" p="xl" radius="md">
            <form onSubmit={handleSubmit} noValidate aria-label={t('login.title')}>
              <Stack gap="md">
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
                  label={t('login.accessKey')}
                  placeholder={t('login.accessKeyPlaceholder')}
                  name="accessKey"
                  autoComplete="username"
                  required
                  value={accessKey}
                  onChange={(event) => {
                    setAccessKey(event.currentTarget.value);
                  }}
                  onBlur={() => {
                    setTouched((prev) => ({ ...prev, accessKey: true }));
                  }}
                  error={fieldError(validation.accessKey, touched.accessKey)}
                  disabled={submitting}
                  data-autofocus
                />
                <PasswordInput
                  label={t('login.secretKey')}
                  placeholder={t('login.secretKeyPlaceholder')}
                  name="secretKey"
                  autoComplete="current-password"
                  required
                  value={secretKey}
                  onChange={(event) => {
                    setSecretKey(event.currentTarget.value);
                  }}
                  onBlur={() => {
                    setTouched((prev) => ({ ...prev, secretKey: true }));
                  }}
                  error={fieldError(validation.secretKey, touched.secretKey)}
                  disabled={submitting}
                />
                <Button type="submit" fullWidth disabled={!canSubmit} loading={submitting}>
                  {submitting ? t('login.submitting') : t('login.submit')}
                </Button>
              </Stack>
            </form>
          </Paper>
        </Stack>
      </Container>
    </Center>
  );
}
