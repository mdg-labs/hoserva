import fs from 'node:fs';
import path from 'node:path';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// Versioning rules: docs/internal/13-open-questions.md Q90. With no
// versions.json the current docs are the whole docs site, at /docs/. Once
// `docs:version` has made a snapshot, the latest stable version takes /docs/
// and the current docs move to /docs/next/, kept out of search engines. The
// site root and /apps are custom pages beside the docs plugin.
const versionsFile = path.join(__dirname, 'versions.json');
const versions: string[] = fs.existsSync(versionsFile)
  ? JSON.parse(fs.readFileSync(versionsFile, 'utf8'))
  : [];
const hasVersions = versions.length > 0;

const config: Config = {
  title: 'Hoserva',
  tagline: 'An open-source home server platform for mixed-size disks',
  url: 'https://hoserva.dev',
  baseUrl: '/',
  favicon: undefined,
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',
  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },
  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },
  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: 'docs',
          sidebarPath: './sidebars.ts',
          lastVersion: hasVersions ? versions[0] : undefined,
          versions: {
            current: {
              label: 'Next',
              banner: 'unreleased',
              noIndex: hasVersions,
            },
          },
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],
  themes: [
    '@docusaurus/theme-mermaid',
    [
      '@easyops-cn/docusaurus-search-local',
      {
        hashed: true,
        indexDocs: true,
        indexBlog: false,
        indexPages: false,
        docsRouteBasePath: 'docs',
      },
    ],
  ],
  themeConfig: {
    navbar: {
      title: 'Hoserva',
      items: [
        ...(hasVersions
          ? [{type: 'docsVersionDropdown' as const, position: 'right' as const}]
          : []),
        {
          href: 'https://github.com/mdg-labs/hoserva',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    colorMode: {
      respectPrefersColorScheme: true,
    },
    mermaid: {
      theme: {light: 'neutral', dark: 'dark'},
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
