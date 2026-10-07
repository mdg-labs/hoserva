import fs from 'node:fs';
import path from 'node:path';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import type * as OpenApiPlugin from 'docusaurus-plugin-openapi-docs';
import {prismTheme} from './src/lib/prism';

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

// The API reference is generated from the OpenAPI specification (never
// committed): the current docs from the build-time copy of api/openapi.yaml,
// and each version in versions.json from its own versioned_api/openapi-X.Y.yaml.
const apiOptions = {
  hideSendButton: true,
  showSchemas: true,
  sidebarOptions: {groupPathsBy: 'tag', categoryLinkSource: 'tag'},
} satisfies Partial<OpenApiPlugin.Options>;

const apiSpecs: Record<string, OpenApiPlugin.Options> = {
  current: {...apiOptions, specPath: '.openapi/current.yaml', outputDir: 'docs/reference/api'},
};
for (const version of versions) {
  const specPath = `versioned_api/openapi-${version}.yaml`;
  if (!fs.existsSync(path.join(__dirname, specPath))) {
    throw new Error(`versions.json lists ${version}, but ${specPath} is missing: copy api/openapi.yaml there when the version is made`);
  }
  apiSpecs[`v${version}`] = {
    ...apiOptions,
    specPath,
    outputDir: `versioned_docs/version-${version}/reference/api`,
  };
}

const config: Config = {
  title: 'Hoserva',
  tagline: 'An open-source home server platform for mixed-size disks',
  url: 'https://hoserva.dev',
  baseUrl: '/',
  // The icons and screenshots of the catalog export (scripts/devenv/catalog-export),
  // served at /apps/<id>/.
  staticDirectories: ['static', '.catalog/static'],
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
          docItemComponent: '@theme/ApiItem',
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
  plugins: [
    './plugins/tailwind',
    './plugins/catalog',
    'docusaurus-plugin-sass',
    [
      'docusaurus-plugin-openapi-docs',
      {
        id: 'api',
        docsPluginId: 'classic',
        config: apiSpecs,
      },
    ],
  ],
  themes: [
    'docusaurus-theme-openapi-docs',
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
        {to: '/apps', label: 'Apps', position: 'left'},
        {to: '/docs/', label: 'Docs', position: 'left'},
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
    prism: {
      theme: prismTheme,
      darkTheme: prismTheme,
    },
    mermaid: {
      theme: {light: 'neutral', dark: 'dark'},
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
