import { ConflictException, Injectable } from '@nestjs/common';
import { Users } from '@prisma/client';

import { ConfigService } from '@nestjs/config';
import { PrismaService } from '../prisma.service';

@Injectable()
export class UsersService {
  constructor(
    private prisma: PrismaService,
    private configService: ConfigService
  ) {}

  async getAllUser(): Promise<Users[]> {
    return this.prisma.users.findMany();
  }
  async createUser(data: Users): Promise<Users> {
    const existingUser = await this.prisma.users.findUnique({
      where: {
        user_name: data.user_name
      }
    });

    if (existingUser) {
      throw new ConflictException('Username already exists');
    }
    const existingEmail = await this.prisma.users.findUnique({
      where: {
        email: data.email
      }
    });
    if (existingEmail) {
      throw new ConflictException('Email already exists');
    }
    const user = await this.prisma.users.create({
      data
    });
    return user;
  }
}
