import fs from 'node:fs';
import path from 'node:path';
import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

type Item = Extract<SidebarsConfig[string], unknown[]>[number];

const docsDir = path.join(__dirname, 'docs');

function isDraft(id: string): boolean {
  const file = ['.md', '.mdx']
    .map((ext) => path.join(docsDir, id + ext))
    .find((candidate) => fs.existsSync(candidate));
  if (file === undefined) {
    throw new Error(`sidebars.ts: no page docs/${id}.md or docs/${id}.mdx`);
  }
  const frontMatter = /^---\r?\n([\s\S]*?)\r?\n---/.exec(fs.readFileSync(file, 'utf8'));
  return frontMatter !== null && /^draft:\s*true\s*$/m.test(frontMatter[1]);
}

// A draft page is left out of a production build, so it gets a label with no
// link instead of a dead one. Removing `draft: true` from the page turns the
// entry into a real link; this file needs no edit.
function page(id: string, label: string): Item {
  if (isDraft(id)) {
    return {
      type: 'html',
      value: `<span class="sidebar-coming-soon">${label}<small>coming soon</small></span>`,
    };
  }
  return {type: 'doc', id, label};
}

function section(label: string, items: Item[]): Item {
  return {type: 'category', label, collapsed: false, items};
}

// The navigation tree is doc 05 §7's structure.
const sidebars: SidebarsConfig = {
  docs: [
    page('index', 'Home'),
    section('Getting started', [
      page('getting-started/requirements', 'Requirements'),
      page('getting-started/install-deb', 'Install on Debian'),
      page('getting-started/install-iso', 'Install from the ISO'),
      page('getting-started/first-array', 'Create your first array'),
    ]),
    section('Migrating from Unraid', [
      page('migrating-from-unraid/overview', 'Overview'),
      page('migrating-from-unraid/before-you-start', 'Before you start'),
      page('migrating-from-unraid/the-migration', 'The migration'),
      page('migrating-from-unraid/after-the-migration', 'After the migration'),
      page('migrating-from-unraid/rollback', 'Rollback'),
      section('Special cases', [
        page('migrating-from-unraid/special-cases/dual-parity', 'Dual parity'),
        page('migrating-from-unraid/special-cases/btrfs-and-ext4-data-disks', 'btrfs and ext4 data disks'),
        page('migrating-from-unraid/special-cases/named-pools', 'Named pools'),
        page('migrating-from-unraid/special-cases/no-cache-disk', 'No cache disk'),
        page('migrating-from-unraid/special-cases/unraid-7', 'Unraid 7'),
        page('migrating-from-unraid/special-cases/unsupported-arrays', 'Unsupported arrays'),
      ]),
      page('migrating-from-unraid/troubleshooting', 'Troubleshooting'),
    ]),
    section('Concepts', [
      page('concepts/how-pooling-works', 'How pooling works'),
      page('concepts/how-parity-works', 'How parity works'),
      page('concepts/parity-is-not-backup', 'Parity is not backup'),
      page('concepts/cache-and-mover', 'Cache and mover'),
      page('concepts/what-happens-when-a-disk-dies', 'What happens when a disk dies'),
      page('concepts/why-disks-wake-up', 'Why disks wake up'),
    ]),
    section('Guides', [
      page('guides/adding-a-disk', 'Adding a disk'),
      page('guides/replacing-a-failed-disk', 'Replacing a failed disk'),
      page('guides/recovering-files', 'Recovering files'),
      page('guides/backing-up-appdata', 'Backing up appdata'),
      page('guides/backing-up-your-data', 'Backing up your data'),
      page('guides/migrating-vms', 'Migrating VMs'),
      page('guides/exposing-safely', 'Exposing services safely'),
    ]),
    section('Reference', [
      page('reference/cli', 'CLI'),
      page('reference/api', 'API'),
      page('reference/config-files', 'Config files'),
      page('reference/template-format', 'Template format'),
    ]),
  ],
};

export default sidebars;
