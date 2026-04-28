import { Injectable, Module } from '@nestjs/common';

/**
 * HealthState is the single signal the readiness probe reads. main.ts
 * flips it to true after both the Fastify HTTP listener and the NATS
 * microservice have finished bootstrap.
 */
@Injectable()
export class HealthState {
  private ready = false;

  public markReady(): void {
    this.ready = true;
  }

  public isReady(): boolean {
    return this.ready;
  }
}

@Module({
  providers: [HealthState],
  exports: [HealthState],
})
export class HealthStateModule {}
