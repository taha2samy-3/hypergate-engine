import type {ReactNode} from 'react';
import clsx from 'clsx';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type FeatureItem = {
  title: string;
  Svg: React.ComponentType<React.ComponentProps<'svg'>>;
  description: ReactNode;
};

const FeatureList: FeatureItem[] = [
  {
    title: 'High Performance',
    Svg: require('@site/static/img/feature_performance.svg').default,
    description: (
      <>
        Hypergate Engine is designed from the ground up to be ultra-fast. Using a pre-warmed request context and efficient gRPC streams, it operates at microsecond latencies.
      </>
    ),
  },
  {
    title: 'Built on Envoy',
    Svg: require('@site/static/img/feature_envoy.svg').default,
    description: (
      <>
        Fully compliant with Envoy's <code>ext_proc</code> API, allowing you to easily inject authentication, ratelimiting, and modifications seamlessly.
      </>
    ),
  },
  {
    title: 'Kubernetes Native',
    Svg: require('@site/static/img/feature_k8s.svg').default,
    description: (
      <>
        Use the Hypergate Operator to deploy CRDs and instantly enforce security policies across your microservices without restarting pods.
      </>
    ),
  },
];

function Feature({title, Svg, description}: FeatureItem) {
  return (
    <div className={clsx('col col--4')}>
      <div className="text--center">
        <Svg className={styles.featureSvg} role="img" />
      </div>
      <div className="text--center padding-horiz--md">
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
