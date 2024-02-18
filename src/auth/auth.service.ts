import {
  ForbiddenException,
  Injectable,
  UnauthorizedException
} from '@nestjs/common';
import { UsersService } from '../users/users.service';
import { PrismaService } from '../prisma.service';
import { JwtService } from '@nestjs/jwt';
import { RegisterUserDto } from './dto/register-user.dto';
import { Users } from '../users/users.model';
import * as bcrypt from 'bcrypt';
import { LoginUserDto } from './dto';
import { Tokens } from './types/tokens.type';

const saltRounds = 10;

@Injectable()
export class AuthService {
  constructor(
    private readonly prismaService: PrismaService,
    private readonly jwtService: JwtService,
    private readonly usersService: UsersService
  ) {}
  async hashPassword(password: string): Promise<string> {
    const salt = await bcrypt.genSalt(saltRounds);
    const hashedPassword = await bcrypt.hash(password, salt);
    return hashedPassword;
  }
  async register(createDto: RegisterUserDto): Promise<Tokens> {
    const createUser = new Users();
    createUser.user_name = createDto.user_name;
    createUser.email = createDto.email;

    createUser.password_hash = await this.hashPassword(createDto.password_hash);
    const user = await this.usersService.createUser(createUser);
    const tokens = await this.getTokens(user.id, user.email);
    await this.updateRtHash(user.id, tokens.refresh_token);
    return tokens;
  }
  async hashData(data: string) {
    return bcrypt.hash(data, saltRounds);
  }
  async updateRtHash(userId: string, rt: string) {
    const hash = await this.hashData(rt);
    await this.prismaService.users.update({
      where: {
        id: userId
      },
      data: {
        hashed_rt: hash
      }
    });
  }
  async getTokens(userId: string, email: string) {
    const [at, rt] = await Promise.all([
      this.jwtService.signAsync(
        {
          sub: userId,
          email
        },
        {
          expiresIn: 60 * 15, //15 min
          secret: process.env.JWT_AT_SECRET
        }
      ),
      this.jwtService.signAsync(
        {
          sub: userId,
          email
        },
        {
          expiresIn: 60 * 60 * 24 * 7, //7 day
          secret: process.env.JWT_RT_SECRET
        }
      )
    ]);
    return {
      access_token: at,
      refresh_token: rt
    };
  }
  async logout(userId: string) {
    await this.prismaService.users.updateMany({
      where: {
        id: userId,
        hashed_rt: {
          not: null
        }
      },
      data: {
        hashed_rt: null
      }
    });
  }
  // async refreshTokens(userId: string, rt: string): Promise<Tokens> {
  //   const user = await this.prismaService.users.findUnique({
  //     where: {
  //       id: userId
  //     }
  //   });

  //   if (!user || !user.hashed_rt) {
  //     throw new ForbiddenException('Access Denied');
  //   }

  //   const rtMatches = await bcrypt.compare(rt, user.hashed_rt);
  //   if (!rtMatches) {
  //     throw new ForbiddenException('Access Denied');
  //   }

  //   const tokens = await this.getTokens(user.id, user.email);
  //   await this.updateRtHash(user.id, tokens.refresh_token);

  //   return tokens;
  // }

  async login(loginDto: LoginUserDto): Promise<Tokens> {
    const { email, password_hash } = loginDto;
    const users = await this.prismaService.users.findUnique({
      where: { email }
    });

    const validatePassword = await bcrypt.compare(
      password_hash,
      users!.password_hash
    );
    if (!validatePassword || !users) {
      throw new UnauthorizedException(
        'User not found or password is incorrect'
      );
    }
    const tokens = await this.getTokens(users.id, users.email);
    return tokens;
  }
}
