import { Module } from '@nestjs/common';
import { AuthService } from './auth.service';
import { AuthController } from './auth.controller';
import { PrismaService } from '../prisma.service';
import { UsersModule } from '../users/users.module';
import { UsersService } from '../users/users.service';
import { PassportModule } from '@nestjs/passport';
import { JwtModule } from '@nestjs/jwt';
import { AtStrategy, RtStrategy } from './strategies';

@Module({
  controllers: [AuthController],
  imports: [UsersModule, PassportModule, JwtModule.register({})],
  providers: [AuthService, PrismaService, UsersService, AtStrategy, RtStrategy]
})
export class AuthModule {}
