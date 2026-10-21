import {
  Anchor,
  AppShell,
  Burger,
  Group,
  Menu,
  NavLink,
  ScrollArea,
  Text,
  Title,
  UnstyledButton,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconChevronDown, IconLogout, IconUserCircle } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Link, Outlet, useLocation, useNavigate } from 'react-router';

import { useSession } from '@/auth/SessionProvider';
import { usePermissions } from '@/auth/usePermissions';
import { ColorSchemeToggle } from '@/components/ColorSchemeToggle';
import { LanguageSwitcher } from '@/components/LanguageSwitcher';
import { LegalNotice } from '@/components/LegalNotice';
import { UploadPanel } from '@/upload/UploadPanel';

import { isNavItemActive, navigation, visibleNavigation } from './navigation';

/** Header + sidebar frame of every authenticated page. */
export function AppShellLayout() {
  const { t } = useTranslation();
  const [opened, navbar] = useDisclosure(false);
  const location = useLocation();
  const navigate = useNavigate();
  const { session, logout } = useSession();
  // A federated user is named by its provider, not by its temporary key.
  const displayName = session?.identity.username ?? session?.accessKey ?? '';
  const permissions = usePermissions();

  const handleLogout = async () => {
    await logout();
    await navigate('/login', { replace: true });
  };

  return (
    <AppShell
      header={{ height: 56 }}
      navbar={{ width: 240, breakpoint: 'sm', collapsed: { mobile: !opened } }}
      padding="md"
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap">
            <Burger
              opened={opened}
              onClick={() => {
                navbar.toggle();
              }}
              hiddenFrom="sm"
              size="sm"
              aria-label={opened ? t('common.closeNavigation') : t('common.openNavigation')}
            />
            <Anchor component={Link} to="/" underline="never" c="inherit">
              <Title order={3}>{t('app.name')}</Title>
            </Anchor>
            <Text size="sm" c="dimmed" visibleFrom="md">
              {t('app.tagline')}
            </Text>
          </Group>
          <Group gap="sm" wrap="nowrap">
            <LanguageSwitcher />
            <ColorSchemeToggle />
            <Menu position="bottom-end" withinPortal>
              <Menu.Target>
                <UnstyledButton aria-label={t('common.signedInAs', { user: displayName })}>
                  <Group gap={6} wrap="nowrap">
                    <IconUserCircle size={20} />
                    <Text size="sm" visibleFrom="xs">
                      {displayName}
                    </Text>
                    <IconChevronDown size={14} />
                  </Group>
                </UnstyledButton>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Label>{t('common.signedInAs', { user: displayName })}</Menu.Label>
                <Menu.Item leftSection={<IconLogout size={16} />} onClick={handleLogout}>
                  {t('common.signOut')}
                </Menu.Item>
              </Menu.Dropdown>
            </Menu>
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs" component="nav" aria-label={t('common.mainNavigation')}>
        <ScrollArea type="auto" style={{ flex: 1 }}>
          {visibleNavigation(navigation, permissions).map((item) => (
            <NavLink
              key={item.key}
              component={Link}
              to={item.path}
              label={t(`nav.${item.key}`)}
              leftSection={<item.icon size={18} stroke={1.6} />}
              active={isNavItemActive(item, location.pathname)}
              onClick={() => {
                navbar.close();
              }}
            />
          ))}
        </ScrollArea>
        <AppShell.Section pt="xs">
          <LegalNotice />
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>
        <Outlet />
        <UploadPanel />
      </AppShell.Main>
    </AppShell>
  );
}
