import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import TrademarkNotice from '@site/src/components/TrademarkNotice';
import {Badge} from '@site/src/components/ui/badge';
import {Button} from '@site/src/components/ui/button';
import {
  Card,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@site/src/components/ui/card';

type Topic = {
  label?: string;
  title: string;
  text: string;
  link: string;
  linkText: string;
};

const capabilities: Topic[] = [
  {
    label: 'Pool',
    title: 'One pool from mixed-size disks',
    text: 'Combine disks of different sizes into one set of shared folders. Adding a disk makes its space available at once, with no rebuild.',
    link: '/docs/concepts/how-pooling-works',
    linkText: 'How pooling works',
  },
  {
    label: 'Parity',
    title: 'Parity on a schedule',
    text: 'One or two parity disks let Hoserva rebuild a failed data disk. A nightly sync updates parity, and a threshold guard holds the sync when a change looks like a mass deletion.',
    link: '/docs/concepts/how-parity-works',
    linkText: 'How parity works',
  },
  {
    label: 'Cache',
    title: 'A cache disk and a mover',
    text: 'Write new files to an SSD first and let the mover relocate them to the array later. App data can stay on the cache so the array disks keep sleeping.',
    link: '/docs/concepts/cache-and-mover',
    linkText: 'Cache and mover',
  },
  {
    label: 'Apps',
    title: 'Apps from a curated catalog',
    text: 'Install self-hosted apps from a catalog of templates written from each application’s own documentation, with the app data kept on the cache and the media on the pool.',
    link: '/docs/guides/backing-up-your-data',
    linkText: 'Install a backup app',
  },
];

const audience: Topic[] = [
  {
    title: 'You run a server with disks of different sizes',
    text: 'You have collected disks over the years and want one pool without a matching-size rule. You can install Debian, and you want a web UI and a command-line tool over standard tools, not hand-maintained mergerfs policies, SnapRAID schedules and Samba configuration.',
    link: '/docs/getting-started/requirements',
    linkText: 'Check the requirements',
  },
  {
    title: 'You are moving from Unraid',
    text: 'Hoserva can adopt the data disks of an existing Unraid array in place, without copying your data, and recreate your shares, accounts and containers on Debian. The overview says what carries over, what does not, and what the move costs in downtime.',
    link: '/docs/migrating-from-unraid/overview',
    linkText: 'Read the migration overview',
  },
];

const limits: Topic[] = [
  {
    title: 'Not real-time parity',
    text: 'Parity is updated by a scheduled sync, by default every night, not on every write. Files written since the last sync are not covered until the next one finishes. This keeps your data disks asleep most of the day.',
    link: '/docs/concepts/how-parity-works',
    linkText: 'Why parity is scheduled',
  },
  {
    title: 'Not a backup',
    text: 'Parity protects you from a failed disk. It does not protect you from a deleted folder, ransomware, a fire or a theft. Keep a second copy of anything you cannot replace.',
    link: '/docs/concepts/parity-is-not-backup',
    linkText: 'Parity is not backup',
  },
  {
    title: 'Not a general container manager',
    text: 'App support is narrow on purpose. It takes you from choosing an application to having it running. It is not a tool for managing arbitrary containers, images and networks.',
    link: '/docs/guides/backing-up-your-data',
    linkText: 'See how an app is installed',
  },
  {
    title: 'Not a hosted service',
    text: 'Hoserva is software you install on your own Debian server. Nothing about it runs as a service that someone else hosts.',
    link: '/docs/getting-started/install-deb',
    linkText: 'Install on Debian',
  },
];

const steps = [
  {
    link: '/docs/getting-started/requirements',
    title: 'Check the requirements',
    text: 'A Debian 13 server with its own boot device, plus the disks for your data and parity.',
  },
  {
    link: '/docs/getting-started/install-deb',
    title: 'Install on Debian',
    text: 'Install the .deb package, then sign in from a browser on your network.',
  },
  {
    link: '/docs/getting-started/first-array',
    title: 'Create your first array',
    text: 'Choose data, parity and cache disks in the setup wizard and start the pool.',
  },
];

function TopicCard({topic}: {topic: Topic}): ReactNode {
  return (
    <Card className="h-full">
      <CardHeader>
        {topic.label !== undefined && (
          <div>
            <Badge variant="outline">{topic.label}</Badge>
          </div>
        )}
        <CardTitle className="text-lg">{topic.title}</CardTitle>
        <CardDescription className="text-base leading-relaxed">
          {topic.text}
        </CardDescription>
      </CardHeader>
      <CardFooter className="mt-auto">
        <Link
          to={topic.link}
          className="text-sm font-medium underline underline-offset-4">
          {topic.linkText} →
        </Link>
      </CardFooter>
    </Card>
  );
}

function Section({
  id,
  title,
  intro,
  children,
}: {
  id: string;
  title: string;
  intro?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <section aria-labelledby={id} className="py-10 sm:py-14">
      <h2 id={id} className="mb-3 text-2xl font-semibold sm:text-3xl">
        {title}
      </h2>
      {intro !== undefined && (
        <p className="mb-8 max-w-2xl text-base text-muted-foreground">
          {intro}
        </p>
      )}
      {children}
    </section>
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="Home server platform for mixed-size disks"
      description="Hoserva is an open-source home server platform for mixed-size disks. It manages a mergerfs pool and SnapRAID parity from a web UI and a command-line tool on Debian.">
      <main className="mx-auto w-full max-w-5xl px-4 sm:px-6">
        <section className="py-14 sm:py-20">
          <Badge variant="outline" size="lg">
            Open source · AGPL-3.0
          </Badge>
          <h1 className="mt-5 mb-5 max-w-3xl text-4xl font-semibold leading-tight sm:text-5xl">
            A home server platform for mixed-size disks
          </h1>
          <p className="mb-8 max-w-2xl text-lg text-muted-foreground">
            Hoserva manages a mergerfs pool and SnapRAID parity from a web UI
            and a command-line tool on Debian. Use data disks of any size,
            protect them with one or two parity disks, and run your apps from a
            curated catalog.
          </p>
          <div className="flex flex-wrap gap-3">
            <Button
              size="xl"
              render={<Link to="/docs/getting-started/requirements" />}>
              Get started
            </Button>
            <Button size="xl" variant="outline" render={<Link to="/docs/" />}>
              Read the documentation
            </Button>
            <Button size="xl" variant="outline" render={<Link to="/apps" />}>
              Browse apps
            </Button>
          </div>
        </section>

        <Section
          id="what-it-is"
          title="What Hoserva is"
          intro="Hoserva is the management layer over two proven open-source tools: mergerfs, which joins disks into one pool, and SnapRAID, which computes parity. It sets them up, schedules them and watches them. It does not replace them.">
          <div className="grid gap-4 sm:grid-cols-2">
            {capabilities.map((topic) => (
              <TopicCard key={topic.title} topic={topic} />
            ))}
          </div>
        </Section>

        <Section id="who-it-is-for" title="Who it is for">
          <div className="grid gap-4 sm:grid-cols-2">
            {audience.map((topic) => (
              <TopicCard key={topic.title} topic={topic} />
            ))}
          </div>
          <div className="mt-4">
            <TrademarkNotice />
          </div>
        </Section>

        <Section
          id="what-it-is-not"
          title="What Hoserva is not"
          intro="Hoserva has a deliberate scope. These are the limits to know before you decide.">
          <div className="grid gap-4 sm:grid-cols-2">
            {limits.map((topic) => (
              <TopicCard key={topic.title} topic={topic} />
            ))}
          </div>
        </Section>

        <Section id="get-started" title="Get started">
          <Card>
            <CardHeader>
              <CardTitle className="text-lg">Three steps to a running pool</CardTitle>
              <CardDescription className="text-base">
                Installing the package does not touch your data disks. You
                choose them later, in the setup wizard.
              </CardDescription>
            </CardHeader>
            <ol className="m-0 flex list-none flex-col gap-4 px-6 pb-2">
              {steps.map((step, index) => (
                <li key={step.link} className="flex gap-3">
                  <Badge size="lg" variant="secondary" aria-hidden="true">
                    {index + 1}
                  </Badge>
                  <div>
                    <Link
                      to={step.link}
                      className="font-medium underline underline-offset-4">
                      {step.title}
                    </Link>
                    <p className="m-0 text-sm text-muted-foreground">
                      {step.text}
                    </p>
                  </div>
                </li>
              ))}
            </ol>
            <CardFooter className="flex-wrap gap-3">
              <Button render={<Link to="/docs/getting-started/requirements" />}>
                Start with the requirements
              </Button>
              <Button
                variant="outline"
                render={<Link href="https://github.com/mdg-labs/hoserva" />}>
                View the source on GitHub
              </Button>
            </CardFooter>
          </Card>
        </Section>
      </main>
    </Layout>
  );
}
