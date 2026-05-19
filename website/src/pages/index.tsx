import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';

import styles from './index.module.css';

function HomepageHeader() {
  const {siteConfig} = useDocusaurusContext();
  return (
    <header className={clsx(styles.heroBanner)}>
      <div className="container">
        <div className={styles.heroGrid}>
          <div>
            <p className={styles.kicker}>Production Documentation</p>
            <Heading as="h1" className={styles.heroTitle}>
              {siteConfig.title}
            </Heading>
            <p className={styles.heroSubtitle}>{siteConfig.tagline}</p>
            <div className={styles.buttons}>
              <Link className="button button--primary button--lg" to="/docs/">
                Read the docs
              </Link>
              <Link
                className="button button--secondary button--lg"
                to="/docs/getting-started/install">
                Install nyxd
              </Link>
            </div>
          </div>
          <aside className={styles.panel}>
            <p className={styles.panelTitle}>Why nyxd</p>
            <ul>
              <li>Linux-first OCI orchestration with crun.</li>
              <li>Native networking by default; CNI optional.</li>
              <li>Compose workflow, health checks, and API control plane.</li>
            </ul>
          </aside>
        </div>
      </div>
    </header>
  );
}

function ValueSection(): ReactNode {
  return (
    <section className={styles.valueSection}>
      <div className="container">
        <div className={styles.cards}>
          <article className={styles.card}>
            <Heading as="h2">Operator-focused</Heading>
            <p>
              The docs prioritize installation, lifecycle operations, and
              troubleshooting for real Linux hosts.
            </p>
          </article>
          <article className={styles.card}>
            <Heading as="h2">Architecture-aware</Heading>
            <p>
              Every section connects behavior to internals so users can reason
              about runtime, networking, and persistence.
            </p>
          </article>
          <article className={styles.card}>
            <Heading as="h2">API-ready</Heading>
            <p>
              OpenAPI is included for automation and integration paths across
              CLI and control plane usage.
            </p>
          </article>
        </div>
        <div className={styles.ctaRow}>
          <Link
            className="button button--outline button--primary button--lg"
            to="https://github.com/zrougamed/nyxd">
            View repository
          </Link>
          <Link
            className="button button--primary button--lg"
            to="https://raw.githubusercontent.com/zrougamed/nyxd/main/docs/openapi.yaml">
            OpenAPI spec
          </Link>
        </div>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  const {siteConfig} = useDocusaurusContext();
  return (
    <Layout
      title={`${siteConfig.title} Documentation`}
      description="Official documentation for nyxd, the minimal OCI orchestrator.">
      <HomepageHeader />
      <main>
        <ValueSection />
      </main>
    </Layout>
  );
}
