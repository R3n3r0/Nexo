import {
  Alert,
  type CSSVariablesResolver,
  createTheme,
  darken,
  DEFAULT_THEME,
  Drawer,
  mergeThemeOverrides,
  Modal,
  Notification,
  PasswordInput,
  type MantineColorsTuple,
  type MantineThemeOverride,
} from '@mantine/core';

const nexo: MantineColorsTuple = [
  '#edf2ff',
  '#dbe4ff',
  '#bac8ff',
  '#91a7ff',
  '#748ffc',
  '#5c7cfa',
  '#4c6ef5',
  '#4263eb',
  '#3b5bdb',
  '#364fc7',
];

/** Mantine theme shared by the light and dark colour schemes. */
export const theme = createTheme({
  primaryColor: 'nexo',
  colors: { nexo },
  // Shade 8 keeps white text on filled controls and coloured text (links,
  // active navigation) above the WCAG AA contrast of 4.5:1 in both schemes.
  primaryShade: { light: 8, dark: 8 },
  // Transitions are skipped for the users who ask the system for less motion.
  respectReducedMotion: true,
  defaultRadius: 'md',
  fontFamily:
    'Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
  fontFamilyMonospace: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
  headings: { fontWeight: '600' },
});

/** Translated labels of the controls Mantine renders without text. */
export interface ThemeLabels {
  /** Close buttons of modals, drawers, notifications and alerts. */
  close: string;
  /** Show/hide toggle of the password inputs. */
  toggleSecret: string;
  /** Controls of the date pickers. */
  dates: {
    previousMonth: string;
    nextMonth: string;
    previousYear: string;
    nextYear: string;
    previousDecade: string;
    nextDecade: string;
    monthLevel: string;
    yearLevel: string;
    time: string;
    submit: string;
  };
}

/**
 * The theme with accessible names for the icon-only controls that Mantine
 * renders inside its components (close buttons, password visibility
 * toggle, date picker controls), in the current language.
 */
export function localizedTheme(labels: ThemeLabels): MantineThemeOverride {
  return mergeThemeOverrides(theme, {
    components: {
      Modal: Modal.extend({ defaultProps: { closeButtonProps: { 'aria-label': labels.close } } }),
      Drawer: Drawer.extend({ defaultProps: { closeButtonProps: { 'aria-label': labels.close } } }),
      Notification: Notification.extend({
        defaultProps: { closeButtonProps: { 'aria-label': labels.close } },
      }),
      Alert: Alert.extend({ defaultProps: { closeButtonLabel: labels.close } }),
      // Plain props: importing DateTimePicker for its extend() would put
      // @mantine/dates in the initial bundle.
      DateTimePicker: {
        defaultProps: {
          ariaLabels: {
            previousMonth: labels.dates.previousMonth,
            nextMonth: labels.dates.nextMonth,
            previousYear: labels.dates.previousYear,
            nextYear: labels.dates.nextYear,
            previousDecade: labels.dates.previousDecade,
            nextDecade: labels.dates.nextDecade,
            monthLevelControl: labels.dates.monthLevel,
            yearLevelControl: labels.dates.yearLevel,
          },
          timeInputProps: { 'aria-label': labels.dates.time },
          submitButtonProps: { 'aria-label': labels.dates.submit },
        },
      },
      PasswordInput: PasswordInput.extend({
        defaultProps: { visibilityToggleButtonProps: { 'aria-label': labels.toggleSecret } },
      }),
    },
  });
}

/**
 * Darkening of shade 9 that the "light" variant text needs, per palette
 * colour, to reach a 4.5:1 contrast on its tinted background (the default
 * shades of yellow, orange, lime, green and teal are too light).
 */
const lightTextDarkening: Record<string, number> = {
  teal: 0.05,
  green: 0.1,
  orange: 0.15,
  lime: 0.2,
  yellow: 0.3,
};

/**
 * CSS variables raising the contrast of the secondary text (dimmed), of the
 * placeholders and of the text of the "light" variant (badges, light
 * buttons, active links) to WCAG AA in both colour schemes.
 */
export const cssVariablesResolver: CSSVariablesResolver = (current) => {
  const light: Record<string, string> = {
    '--mantine-color-dimmed': '#5c636a',
    '--mantine-color-placeholder': '#666e75',
  };
  for (const [name, shades] of Object.entries({ ...DEFAULT_THEME.colors, ...current.colors })) {
    if (name === 'dark' || shades[9] === undefined) continue;
    light[`--mantine-color-${name}-light-color`] = darken(shades[9], lightTextDarkening[name] ?? 0);
  }
  return {
    variables: {},
    light,
    dark: {
      '--mantine-color-dimmed': 'var(--mantine-color-dark-1)',
      '--mantine-color-placeholder': '#a0a0a0',
    },
  };
};
