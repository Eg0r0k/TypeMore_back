import { Injectable } from '@nestjs/common';
import { PrismaService } from './prisma.service';
import * as bcrypt from 'bcrypt';
import { ConfigService } from '@nestjs/config';
const saltRounds = 10; //Генераций соли

@Injectable()
export class AppService {
  constructor(
    private prisma: PrismaService,
    private configService: ConfigService
  ) {}
  getAll() {
    return this.prisma.users.findMany();
  }
  async hashPassword(password: string): Promise<string> {
    const salt = await bcrypt.genSalt(saltRounds);
    const hashedPassword = await bcrypt.hash(password, salt);
    return hashedPassword;
  }
  async comparePassword(enteredPassword: string, storedPassword: string) {
    return await bcrypt.compare(enteredPassword, storedPassword);
  }
  async findOne(email: string) {
    return await this.prisma.users.findUnique({
      where: {
        email: email
      }
    });
  }
}
