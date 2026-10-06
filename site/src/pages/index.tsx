import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import TrademarkNotice from '@site/src/components/TrademarkNotice';

export default function Home(): ReactNode {
  return (
    <Layout
      title="Hoserva"
      description="An open-source home server platform for mixed-size disks">
      <main className="container margin-vert--xl">
        <h1>Hoserva</h1>
        <p>
          Hoserva is an open-source home server platform for mixed-size disks.
          It manages a mergerfs pool and SnapRAID parity from a web UI and a
          command-line tool on Debian, and includes a guided migration path
          from Unraid.
        </p>
        <p>
          <Link className="button button--primary" to="/docs/">
            Read the documentation
          </Link>
        </p>
        <TrademarkNotice />
      </main>
    </Layout>
  );
}
