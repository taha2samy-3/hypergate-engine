import type {ComponentType, ReactNode, SVGProps} from 'react';
import Link from '@docusaurus/Link';
import Heading from '@theme/Heading';

import RateLimitIcon from '@site/static/img/icons/rate-limit.svg';
import ApiKeyIcon from '@site/static/img/icons/api-key.svg';
import JwtIcon from '@site/static/img/icons/jwt.svg';
import ExternalAuthIcon from '@site/static/img/icons/external-auth.svg';
import FirewallIcon from '@site/static/img/icons/firewall.svg';
import DenyIcon from '@site/static/img/icons/deny.svg';
import HeaderModifierIcon from '@site/static/img/icons/header-modifier.svg';
import CorrelationIcon from '@site/static/img/icons/correlation-id.svg';
import EnricherIcon from '@site/static/img/icons/enricher.svg';
import HotReloadIcon from '@site/static/img/icons/hot-reload.svg';
import FailClosedIcon from '@site/static/img/icons/fail-closed.svg';
import ClientIpIcon from '@site/static/img/icons/client-ip.svg';
import OperatorIcon from '@site/static/img/icons/operator.svg';

import styles from './styles.module.css';

type Card = {
  title: string;
  Icon: ComponentType<SVGProps<SVGSVGElement>>;
  description: ReactNode;
  to: string;
};

const filters: Card[] = [
  {
    title: 'Rate limiting',
    Icon: RateLimitIcon,
    to: '/docs/filters/rate-limiting',
    description: 'Fixed window, sliding window, sliding log, token and leaky bucket, backed by Redis with a local block cache.',
  },
  {
    title: 'API keys',
    Icon: ApiKeyIcon,
    to: '/docs/filters/api-key',
    description: 'Hashed key lookup in Redis, account status checks and metadata injected as upstream headers.',
  },
  {
    title: 'JWT auth',
    Icon: JwtIcon,
    to: '/docs/filters/jwt-auth',
    description: 'Local validation with JWKS refresh or HMAC secrets, optional RFC 7662 introspection, claim mapping.',
  },
  {
    title: 'External auth',
    Icon: ExternalAuthIcon,
    to: '/docs/filters/external-auth',
    description: 'Run oauth2-proxy or your own service as a sidecar, reached over a Unix domain socket.',
  },
  {
    title: 'Firewall',
    Icon: FirewallIcon,
    to: '/docs/filters/firewall',
    description: 'Plug in a WAF sidecar that sees headers and, when enabled, the buffered request body.',
  },
  {
    title: 'Deny',
    Icon: DenyIcon,
    to: '/docs/filters/deny',
    description: 'Block on path, request headers or upstream response headers with a custom status and body.',
  },
  {
    title: 'Header modifier',
    Icon: HeaderModifierIcon,
    to: '/docs/filters/header-modifier',
    description: 'Add, override and remove headers on the way to the upstream and on the way back to the client.',
  },
  {
    title: 'Correlation ID',
    Icon: CorrelationIcon,
    to: '/docs/filters/correlation-id',
    description: 'Generate or validate request IDs (UUIDv4/v7, ULID, XID) and propagate them both ways.',
  },
  {
    title: 'Metadata enricher',
    Icon: EnricherIcon,
    to: '/docs/filters/redis-metadata-enricher',
    description: 'Look up JSON metadata in Redis from request attributes and inject selected fields.',
  },
];

const guarantees: Card[] = [
  {
    title: 'Transactional hot reload',
    Icon: HotReloadIcon,
    to: '/docs/concepts/hot-reload',
    description: 'A new policy is compiled completely before it is published. In-flight requests keep the policy they started with.',
  },
  {
    title: 'Fails closed',
    Icon: FailClosedIcon,
    to: '/docs/concepts/failure-modes',
    description: 'Routes to missing or degraded chains are rejected instead of silently skipping their policy.',
  },
  {
    title: 'Trustworthy client IP',
    Icon: ClientIpIcon,
    to: '/docs/concepts/client-ip',
    description: "Derived from Envoy's peer address and a configured number of trusted proxy hops, never from raw headers.",
  },
  {
    title: 'Kubernetes operator',
    Icon: OperatorIcon,
    to: '/docs/getting-started/installation',
    description: 'Declare filters, chains and routes as CRDs. The operator compiles them and manages the engine DaemonSet.',
  },
];

function CardGrid({cards}: {cards: Card[]}) {
  return (
    <div className={styles.grid}>
      {cards.map(({title, Icon, description, to}) => (
        <Link key={title} to={to} className={styles.card}>
          <span className={styles.iconBadge}>
            <Icon className={styles.icon} aria-hidden="true" />
          </span>
          <Heading as="h3" className={styles.cardTitle}>
            {title}
          </Heading>
          <p className={styles.cardText}>{description}</p>
        </Link>
      ))}
    </div>
  );
}

export default function HomepageFeatures(): ReactNode {
  return (
    <>
      <section className={styles.section}>
        <div className="container">
          <Heading as="h2" className={styles.sectionTitle}>
            Filters
          </Heading>
          <p className={styles.sectionLead}>Compose them into chains and attach chains to routes.</p>
          <CardGrid cards={filters} />
        </div>
      </section>
      <section className={styles.section}>
        <div className="container">
          <Heading as="h2" className={styles.sectionTitle}>
            Built for production
          </Heading>
          <CardGrid cards={guarantees} />
        </div>
      </section>
    </>
  );
}
