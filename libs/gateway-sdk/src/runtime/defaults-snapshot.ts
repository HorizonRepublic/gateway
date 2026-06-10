import type { IGatewayDefaults } from '../types';

/**
 * Module-scoped snapshot of the `IGatewayDefaults` installed by
 * `GatewayModule.forRoot` / `forRootAsync`.
 * @remarks
 * Deliberately NOT a DI provider: the consumer is the lazy `meta` getter
 * composed by `@GatewayRoute`, which lives outside the DI graph and is
 * read by the transport at `listen()` time. Holding the snapshot at
 * module scope removes every lifecycle-ordering dependency between
 * module init and metadata publication.
 *
 * Known limitation: two Nest apps with different `forRoot` defaults in
 * one process share the last-written snapshot. Pattern-extras metadata
 * on controller prototypes is process-global already, so per-app
 * isolation was never possible at this layer.
 */
let snapshot: IGatewayDefaults = {};

/** Install the active defaults snapshot. Last write wins. */
export const setDefaultsSnapshot = (defaults: IGatewayDefaults): void => {
  snapshot = defaults;
};

/** Read the active defaults snapshot; `{}` until one is installed. */
export const getDefaultsSnapshot = (): IGatewayDefaults => snapshot;
