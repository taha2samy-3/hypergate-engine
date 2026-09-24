import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

const repoUrl = 'https://github.com/taha2samy-3/hypergate-engine';

const config: Config = {
  title: 'Hypergate',
  tagline:
    'A policy engine for Envoy: authentication, rate limiting and traffic rules, declared as Kubernetes resources and enforced over ext_proc.',
  favicon: 'img/favicon.svg',

  future: {
    v4: true,
  },

  url: 'https://taha2samy-3.github.io',
  baseUrl: '/hypergate-engine/',
  organizationName: 'taha2samy-3',
  projectName: 'hypergate-engine',
  trailingSlash: false,

  onBrokenLinks: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  stylesheets: [
    'https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;600&display=swap',
  ],

  headTags: [
    {tagName: 'meta', attributes: {name: 'theme-color', content: '#0b1026'}},
  ],

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          editUrl: `${repoUrl}/tree/main/website/`,
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/hypergate-social-card.png',
    colorMode: {
      defaultMode: 'dark',
      disableSwitch: false,
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'Hypergate',
      logo: {
        alt: 'Hypergate logo',
        src: 'img/logo.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
        },
        {to: '/docs/filters/rate-limiting', label: 'Filters', position: 'left'},
        {to: '/docs/reference/engine-configuration', label: 'Reference', position: 'left'},
        {
          href: repoUrl,
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      logo: {
        alt: 'Hypergate',
        src: 'img/logo-wordmark.svg',
        width: 180,
      },
      links: [
        {
          title: 'Start',
          items: [
            {label: 'Introduction', to: '/docs/intro'},
            {label: 'Install the operator', to: '/docs/getting-started/installation'},
            {label: 'Kubernetes quickstart', to: '/docs/getting-started/quickstart-kubernetes'},
          ],
        },
        {
          title: 'Concepts',
          items: [
            {label: 'Architecture', to: '/docs/concepts/architecture'},
            {label: 'Routing', to: '/docs/concepts/routing'},
            {label: 'Failure modes', to: '/docs/concepts/failure-modes'},
          ],
        },
        {
          title: 'Project',
          items: [
            {label: 'GitHub', href: repoUrl},
            {label: 'Issues', href: `${repoUrl}/issues`},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Hypergate contributors.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.oneDark,
      additionalLanguages: ['bash', 'yaml', 'go'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
