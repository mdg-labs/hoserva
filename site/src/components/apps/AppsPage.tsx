import {useEffect, useMemo, useState, type ReactNode} from 'react';
import Link from '@docusaurus/Link';
import {useLocation} from '@docusaurus/router';
import Layout from '@theme/Layout';
import {Badge} from '@site/src/components/ui/badge';
import {Button} from '@site/src/components/ui/button';
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@site/src/components/ui/card';
import {Input} from '@site/src/components/ui/input';
import {AppIcon} from './AppIcon';
import {builtOn, categoryLabel, matches, type AppEntry, type Catalog} from './types';

function AppCard({app}: {app: AppEntry}): ReactNode {
  return (
    <li className="m-0 list-none">
      <Card className="relative h-full">
        <CardHeader>
          <div className="flex items-center gap-3">
            <AppIcon app={app} />
            <CardTitle className="text-lg">
              <Link
                to={`/apps/${app.id}`}
                className="after:absolute after:inset-0 after:rounded-2xl hover:underline">
                {app.title}
              </Link>
            </CardTitle>
          </div>
          {app.description !== undefined && (
            <CardDescription className="line-clamp-3 text-base leading-relaxed">
              {app.description}
            </CardDescription>
          )}
          <div className="flex flex-wrap gap-1.5">
            {app.categories.map((category) => (
              <Badge key={category} variant="outline">
                {categoryLabel(category)}
              </Badge>
            ))}
          </div>
        </CardHeader>
      </Card>
    </li>
  );
}

export default function AppsPage({catalog}: {catalog: Catalog}): ReactNode {
  const {search} = useLocation();
  const [interactive, setInteractive] = useState(false);
  const [query, setQuery] = useState('');
  const [category, setCategory] = useState('');

  const categories = useMemo(
    () => Array.from(new Set(catalog.templates.flatMap((app) => app.categories))).sort(),
    [catalog],
  );

  useEffect(() => setInteractive(true), []);
  useEffect(() => {
    const wanted = new URLSearchParams(search).get('category');
    if (wanted !== null && categories.includes(wanted)) setCategory(wanted);
  }, [search, categories]);

  const shown = catalog.templates.filter(
    (app) =>
      (category === '' || app.categories.includes(category)) &&
      matches(app, query),
  );
  const filtering = query.trim() !== '' || category !== '';
  const built = builtOn(catalog);

  return (
    <Layout
      title="Apps"
      description="The curated catalog of apps you can install from Hoserva: each template is written from the application's own documentation.">
      <main className="mx-auto w-full max-w-5xl px-4 py-10 sm:px-6 sm:py-14">
        <h1 className="mb-3 text-4xl font-semibold leading-tight sm:text-5xl">
          Apps
        </h1>
        <p className="mb-2 max-w-2xl text-lg text-muted-foreground">
          The curated catalog of apps you can install from Hoserva. Each
          template is written from the application’s own documentation. To
          install one, open <strong>Apps → Catalog</strong> in Hoserva.
        </p>
        <p className="mb-8 text-sm text-muted-foreground">
          Catalog serial {catalog.serial}
          {built !== undefined && <>, built {built}</>}.
        </p>

        {catalog.templates.length === 0 ? (
          <Card>
            <CardHeader>
              <CardTitle className="text-lg">The catalog has no apps yet</CardTitle>
              <CardDescription className="text-base">
                Templates will appear here once the catalog publishes them.
              </CardDescription>
            </CardHeader>
          </Card>
        ) : (
          <>
            {interactive && (
              <div className="mb-6 flex flex-col gap-4">
                <div className="max-w-md">
                  <label htmlFor="apps-search" className="sr-only">
                    Search apps
                  </label>
                  <Input
                    id="apps-search"
                    type="search"
                    nativeInput
                    placeholder="Search by name, description or category"
                    value={query}
                    onChange={(event) => setQuery(event.target.value)}
                  />
                </div>
                <div
                  role="group"
                  aria-label="Filter by category"
                  className="flex flex-wrap gap-2">
                  {['', ...categories].map((name) => (
                    <Button
                      key={name}
                      size="xs"
                      variant={category === name ? 'default' : 'outline'}
                      aria-pressed={category === name}
                      onClick={() => setCategory(name)}>
                      {name === '' ? 'All' : categoryLabel(name)}
                    </Button>
                  ))}
                </div>
              </div>
            )}
            <p role="status" className="mb-4 text-sm text-muted-foreground">
              {filtering
                ? `${shown.length} of ${catalog.templates.length} apps`
                : `${catalog.templates.length} apps`}
            </p>
            {shown.length === 0 ? (
              <Card>
                <CardHeader>
                  <CardTitle className="text-lg">No apps match</CardTitle>
                  <CardDescription className="text-base">
                    The catalog has {catalog.templates.length} apps, but none
                    match this search and category.
                  </CardDescription>
                  <div>
                    <Button
                      variant="outline"
                      onClick={() => {
                        setQuery('');
                        setCategory('');
                      }}>
                      Clear filters
                    </Button>
                  </div>
                </CardHeader>
              </Card>
            ) : (
              <ul className="m-0 grid list-none gap-4 p-0 sm:grid-cols-2 lg:grid-cols-3">
                {shown.map((app) => (
                  <AppCard key={app.id} app={app} />
                ))}
              </ul>
            )}
          </>
        )}
      </main>
    </Layout>
  );
}
