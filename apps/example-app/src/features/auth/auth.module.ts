import { Module } from '@nestjs/common';

import { ArticlesController } from './articles.controller';
import { AuthService } from './auth.service';
import { MeController } from './me.controller';
import { JwtVerifier } from './verifiers/jwt.verifier';
import { SessionVerifier } from './verifiers/session.verifier';

@Module({
  controllers: [JwtVerifier, SessionVerifier, MeController, ArticlesController],
  providers: [AuthService],
})
export class AuthModule {}
