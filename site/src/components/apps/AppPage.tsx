import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import Layout from '@theme/Layout';
import {Badge} from '@site/src/components/ui/badge';
import {Button} from '@site/src/components/ui/button';
import {
  Card,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@site/src/components/ui/card';
import {AppIcon} from './AppIcon';
import {categoryLabel, type AppEntry} from './types';

type Data = {app: AppEntry};

function Screenshot({app, file, number}: {app: AppEntry; file: string; number: number}): ReactNode {
  const src = useBaseUrl(`/apps/${app.id}/${file}`);
  return (
    <li className="m-0 list-none">
      <img
        src={src}
        alt={`${app.title} screenshot ${number}`}
        loading="lazy"
        className="w-full rounded-xl border"
      />
    </li>
  );
}

export default function AppPage({data}: {data: Data}): ReactNode {
  const {app} = data;

  return (
    <Layout
      title={app.title}
      description={app.description ?? `${app.title} in Hoserva's curated app catalog.`}>
      <main className="mx-auto w-full max-w-3xl px-4 py-10 sm:px-6 sm:py-14">
        <p className="mb-6 text-sm">
          <Link to="/apps" className="underline underline-offset-4">
            ← All apps
          </Link>
        </p>
        <div className="mb-6 flex items-center gap-4">
          <AppIcon app={app} className="size-16 rounded-2xl" />
          <h1 className="m-0 text-3xl font-semibold leading-tight sm:text-4xl">
            {app.title}
          </h1>
        </div>
        <div className="mb-6 flex flex-wrap gap-1.5">
          {app.categories.map((category) => (
            <Link key={category} to={`/apps?category=${encodeURIComponent(category)}`}>
              <Badge variant="outline" size="lg" render={<span />}>
                {categoryLabel(category)}
              </Badge>
            </Link>
          ))}
        </div>

        {app.description !== undefined && (
          <p className="mb-6 whitespace-pre-line text-base leading-relaxed">
            {app.description}
          </p>
        )}

        <dl className="mb-8 grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
          {app.maintainer !== undefined && (
            <>
              <dt className="text-muted-foreground">Maintainer</dt>
              <dd className="m-0">{app.maintainer}</dd>
            </>
          )}
          <dt className="text-muted-foreground">Documentation</dt>
          <dd className="m-0">
            <Link href={app.docs} className="break-all underline underline-offset-4">
              {app.docs}
            </Link>
          </dd>
          <dt className="text-muted-foreground">Template revision</dt>
          <dd className="m-0">{app.revision}</dd>
        </dl>

        {app.screenshots.length > 0 && (
          <section aria-labelledby="screenshots" className="mb-8">
            <h2 id="screenshots" className="mb-4 text-2xl font-semibold">
              Screenshots
            </h2>
            <ul className="m-0 grid list-none gap-4 p-0 sm:grid-cols-2">
              {app.screenshots.map((file, index) => (
                <Screenshot key={file} app={app} file={file} number={index + 1} />
              ))}
            </ul>
          </section>
        )}

        <Card>
          <CardHeader>
            <CardTitle className="text-lg">Install {app.title}</CardTitle>
            <CardDescription className="text-base">
              In Hoserva, open <strong>Apps → Catalog</strong>, select{' '}
              {app.title} and fill in the install form. This page only lists the
              app; nothing here installs it.
            </CardDescription>
          </CardHeader>
          <CardFooter className="flex-wrap gap-3">
            <Button
              variant="outline"
              render={<Link to="/docs/guides/backing-up-your-data" />}>
              See how an app is installed
            </Button>
            <Button variant="ghost" render={<Link to="/apps" />}>
              Browse all apps
            </Button>
          </CardFooter>
        </Card>
      </main>
    </Layout>
  );
}
