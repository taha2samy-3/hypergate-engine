import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';

import Logo from '@site/static/img/logo.svg';
import EnvoyIcon from '@site/static/img/icons/envoy.svg';
import ChainIcon from '@site/static/img/icons/chain.svg';
import FailClosedIcon from '@site/static/img/icons/fail-closed.svg';
import HomepageFeatures from '@site/src/components/HomepageFeatures';

import styles from './index.module.css';

function HomepageHeader() {
  const {siteConfig} = useDocusaurusContext();
  return (
    <header className={styles.hero}>
      <div className={clsx('container', styles.heroInner)}>
        <Logo className={styles.heroLogo} role="img" aria-label="Hypergate logo" />
        <Heading as="h1" className={styles.heroTitle}>
          Hyper<span>gate</span>
        </Heading>
        <p className={styles.heroTagline}>{siteConfig.tagline}</p>
        <div className={styles.buttons}>
          <Link className={clsx('button button--lg', styles.primaryButton)} to="/docs/getting-started/quickstart-kubernetes">
            Get started
          </Link>
          <Link className={clsx('button button--lg', styles.secondaryButton)} to="/docs/concepts/architecture">
            How it works
          </Link>
        </div>
      </div>
    </header>
  );
}

const steps = [
  {
    Icon: EnvoyIcon,
    title: 'Envoy asks',
    text: 'Envoy streams each request to Hypergate over the ext_proc gRPC API before forwarding it.',
  },
  {
    Icon: ChainIcon,
    title: 'Your chain runs',
    text: 'The matching route selects a filter chain: auth, rate limits, header rules, WAF sidecars.',
  },
  {
    Icon: FailClosedIcon,
    title: 'Allow, modify or block',
    text: 'Hypergate returns header mutations or an immediate response. Unknown policy fails closed.',
  },
];

function HowItWorks() {
  return (
    <section className={styles.steps}>
      <div className="container">
        <div className={styles.stepGrid}>
          {steps.map(({Icon, title, text}, i) => (
            <div key={title} className={styles.step}>
              <div className={styles.stepBadge}>
                <Icon className={styles.stepIcon} aria-hidden="true" />
              </div>
              <div>
                <span className={styles.stepNumber}>0{i + 1}</span>
                <Heading as="h3">{title}</Heading>
                <p>{text}</p>
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  const {siteConfig} = useDocusaurusContext();
  return (
    <Layout title="Policy engine for Envoy" description={siteConfig.tagline}>
      <HomepageHeader />
      <main>
        <HowItWorks />
        <HomepageFeatures />
      </main>
    </Layout>
  );
}
