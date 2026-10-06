import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import TrademarkNotice from '@site/src/components/TrademarkNotice';
import {Button} from '@site/src/components/ui/button';
import {Card, CardPanel} from '@site/src/components/ui/card';

export default function Home(): ReactNode {
  return (
    <Layout
      title="Hoserva"
      description="An open-source home server platform for mixed-size disks">
      <main className="container margin-vert--xl">
        <Card>
          <CardPanel>
            <h1>Hoserva</h1>
            <p>
              Hoserva is an open-source home server platform for mixed-size
              disks. It manages a mergerfs pool and SnapRAID parity from a web
              UI and a command-line tool on Debian, and includes a guided
              migration path from Unraid.
            </p>
            <p>
              <Button render={<Link to="/docs/" />}>
                Read the documentation
              </Button>
            </p>
            <TrademarkNotice />
          </CardPanel>
        </Card>
      </main>
    </Layout>
  );
}
