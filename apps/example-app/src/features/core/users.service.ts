import { Injectable, NotFoundException } from '@nestjs/common';

import type { ICreateUserDto, IUpdateUserDto, IUser } from './users.types';

const SEED: readonly IUser[] = [
  { id: 'alice', name: 'Alice', role: 'admin' },
  { id: 'bob', name: 'Bob', role: 'user' },
  { id: 'charlie', name: 'Charlie', role: 'user' },
];

@Injectable()
export class UsersService {
  private readonly users = new Map<string, IUser>();
  private serial = 0;

  public constructor() {
    this.reset();
  }

  public findById(id: string): IUser | undefined {
    return this.users.get(id);
  }

  public list(roles?: IUser['role'][]): IUser[] {
    const all = [...this.users.values()].sort((a, b) => a.id.localeCompare(b.id));

    if (roles === undefined || roles.length === 0) {
      return all;
    }

    const allowed = new Set(roles);

    return all.filter((u) => allowed.has(u.role));
  }

  public create(dto: ICreateUserDto): IUser {
    const user: IUser = {
      id: this.generateId(),
      name: dto.name,
      role: dto.role ?? 'user',
    };

    this.users.set(user.id, user);
    return user;
  }

  public update(id: string, dto: IUpdateUserDto): IUser {
    const existing = this.users.get(id);

    if (existing === undefined) {
      throw new NotFoundException(`user ${id} not found`);
    }

    const next: IUser = {
      ...existing,
      ...(dto.name !== undefined ? { name: dto.name } : {}),
      ...(dto.role !== undefined ? { role: dto.role } : {}),
    };

    this.users.set(id, next);
    return next;
  }

  public delete(id: string): void {
    if (!this.users.has(id)) {
      throw new NotFoundException(`user ${id} not found`);
    }

    this.users.delete(id);
  }

  // reset clears state and re-seeds the canonical fixture set. Called at
  // construction and from POST /__e2e/reset between e2e tests that
  // mutate state. Resets the id serial so generated ids stay stable
  // within one test process even after many resets, and copies seed
  // entries by value so a later in-place mutation of a returned user
  // cannot corrupt the seed.
  public reset(): void {
    this.users.clear();
    this.serial = 0;
    for (const seed of SEED) {
      this.users.set(seed.id, { ...seed });
    }
  }

  private generateId(): string {
    this.serial += 1;
    return `e2e-${Date.now()}-${this.serial}`;
  }
}
