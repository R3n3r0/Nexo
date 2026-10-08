import { Alert, Button, Code, Container, Group, Stack, Text, Title } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { Component, type ErrorInfo, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

interface ErrorBoundaryProps {
  children: ReactNode;
  /** Optional hook for error reporting. */
  onError?: (error: Error, info: ErrorInfo) => void;
}

interface ErrorBoundaryState {
  error: Error | null;
}

/**
 * Last line of defence: catches render errors anywhere in the tree and
 * shows a recoverable error page instead of a blank screen.
 */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  override state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    this.props.onError?.(error, info);
    console.error('Unhandled render error', error, info.componentStack);
  }

  private readonly reset = () => {
    this.setState({ error: null });
  };

  override render(): ReactNode {
    if (this.state.error) {
      return <ErrorFallback error={this.state.error} onReset={this.reset} />;
    }
    return this.props.children;
  }
}

export interface ErrorFallbackProps {
  error: Error;
  onReset?: () => void;
}

export function ErrorFallback({ error, onReset }: ErrorFallbackProps) {
  const { t } = useTranslation();
  return (
    <Container size="sm" py="xl">
      <Stack gap="md">
        <Alert
          color="red"
          variant="light"
          icon={<IconAlertTriangle size={20} />}
          title={<Title order={3}>{t('errorBoundary.title')}</Title>}
        >
          <Text>{t('errorBoundary.message')}</Text>
        </Alert>
        <details>
          <summary>{t('errorBoundary.details')}</summary>
          <Code block mt="xs">
            {error.stack ?? error.message}
          </Code>
        </details>
        <Group>
          {onReset ? (
            <Button variant="default" onClick={onReset}>
              {t('errorBoundary.tryAgain')}
            </Button>
          ) : null}
          <Button
            onClick={() => {
              window.location.reload();
            }}
          >
            {t('errorBoundary.reload')}
          </Button>
        </Group>
      </Stack>
    </Container>
  );
}
